package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPhoneCheck(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		ok     bool
	}{{"valid", 200, string([]byte{0, 1, 2}), true}, {"corrupt", 200, "bad", false}, {"auth", 407, "", false}} {
		t.Run(test.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "Bearer private-token" {
					t.Error("auth missing")
				}
				if r.URL.RawQuery != "bytes=3" {
					t.Error(r.URL)
				}
				w.Header().Set("Content-Length", "3")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer s.Close()
			rate, e := checkPhone(context.Background(), strings.TrimPrefix(s.URL, "http://"), "private-token", 3)
			if (e == nil) != test.ok {
				t.Fatal(e)
			}
			if test.ok && rate <= 0 {
				t.Fatal(rate)
			}
		})
	}
}
func TestPhoneCheckRejectsRemote(t *testing.T) {
	if _, e := checkPhone(context.Background(), "8.8.8.8:80", "token", 3); e == nil {
		t.Fatal("remote accepted")
	}
}

// r12Phone is a loopback stand-in for the iOS proxy: authenticated info, download and
// CONNECT, with every request it receives recorded in order.
type r12Phone struct {
	l             net.Listener
	mu            sync.Mutex
	events        []string
	protocol      int // 0: /diagnostic/info is unknown (404), as on an old app
	connectStatus int // 0: 200
	tlsCfg        *tls.Config
}

func newR12Phone(t *testing.T, protocol int) *r12Phone {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &r12Phone{l: l, protocol: protocol}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *r12Phone) addr() string { return p.l.Addr().String() }
func (p *r12Phone) log(e string) { p.mu.Lock(); p.events = append(p.events, e); p.mu.Unlock() }
func (p *r12Phone) seen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.events, " | ")
}

func (p *r12Phone) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		return
	}
	status := func(code int, body string) {
		fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", code, http.StatusText(code), len(body), body)
	}
	if req.Header.Get("Proxy-Authorization") != "Bearer "+tokenA {
		p.log("denied " + req.Method)
		status(407, "")
		return
	}
	switch {
	case req.Method == http.MethodConnect:
		p.log("connect " + req.Host)
		if p.connectStatus != 0 && p.connectStatus != 200 {
			status(p.connectStatus, "")
			return
		}
		fmt.Fprint(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if p.tlsCfg != nil {
			tc := tls.Server(c, p.tlsCfg)
			_ = tc.Handshake()
		}
	case req.URL.Path == "/diagnostic/info":
		p.log("info")
		if p.protocol == 0 {
			status(404, "")
			return
		}
		status(200, fmt.Sprintf(`{"protocol":%d}`, p.protocol))
	case req.URL.Path == "/diagnostic/download":
		p.log("download")
		n, _ := strconv.Atoi(req.URL.Query().Get("bytes"))
		body := make([]byte, n)
		for i := range body {
			body[i] = byte(i % 251)
		}
		status(200, string(body))
	default:
		status(404, "")
	}
}

// r12Cert is a self-signed certificate for host; the pool trusts it.
func r12Cert(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestPreflightRejectsOldProtocol(t *testing.T) {
	for _, protocol := range []int{0, 1, 3} {
		t.Run(fmt.Sprint("protocol", protocol), func(t *testing.T) {
			p := newR12Phone(t, protocol)
			err := probeStartup(context.Background(), p.addr(), tokenA)
			var pe *phoneCheckError
			if !errors.As(err, &pe) || pe.Stage != stageInfo || errors.Is(err, errPhoneRejected) {
				t.Fatalf("want an info-stage error that is not a token rejection: %v", err)
			}
			if p.seen() != "info" {
				t.Fatalf("nothing may follow a refused protocol: %s", p.seen())
			}
		})
	}
}

func TestPreflightOrder(t *testing.T) {
	cert, pool := r12Cert(t, defaultProbeHost)
	probeRoots = pool
	t.Cleanup(func() { probeRoots = nil })
	p := newR12Phone(t, 2)
	p.tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}}
	if err := probeStartup(context.Background(), p.addr(), tokenA); err != nil {
		t.Fatal(err)
	}
	warn, err := cellularProbe(context.Background(), p.addr(), tokenA, defaultProbeHost)
	if err != nil || warn != nil {
		t.Fatalf("a verified handshake is neither error nor warning: warn=%v err=%v", warn, err)
	}
	if want := "info | download | connect " + defaultProbeHost + ":443"; p.seen() != want {
		t.Fatalf("order = %s, want %s", p.seen(), want)
	}
}

func TestPreflightTLSFailureIsWarnOnly(t *testing.T) {
	cert, _ := r12Cert(t, defaultProbeHost)
	for name, cfg := range map[string]*tls.Config{
		"untrusted certificate (system roots do not hold it)": {Certificates: []tls.Certificate{cert}},
		"host does not speak TLS":                             nil,
	} {
		t.Run(name, func(t *testing.T) {
			p := newR12Phone(t, 2)
			p.tlsCfg = cfg
			warn, err := cellularProbe(context.Background(), p.addr(), tokenA, defaultProbeHost)
			var pe *phoneCheckError
			if err != nil || !errors.As(warn, &pe) || pe.Stage != stageTLS {
				t.Fatalf("TLS failure must be a warning only: warn=%v err=%v", warn, err)
			}
		})
	}
}

func TestPreflightErrorsCarryStageAndNoToken(t *testing.T) {
	for _, status := range []int{403, 407, 502} {
		p := newR12Phone(t, 2)
		p.connectStatus = status
		_, err := cellularProbe(context.Background(), p.addr(), tokenA, defaultProbeHost)
		var pe *phoneCheckError
		if !errors.As(err, &pe) || pe.Stage != stageConnect || pe.Status != status {
			t.Fatalf("status %d: %v", status, err)
		}
		if strings.Contains(err.Error(), tokenA) || strings.Contains(err.Error(), "Bearer") {
			t.Fatalf("error leaks the credential: %v", err)
		}
		if got := asRejection(err); (got == errPhoneRejected) != (status == 407) {
			t.Fatalf("only 407 is a token rejection (status %d): %v", status, got)
		}
	}
	dead := newR12Phone(t, 2)
	addr := dead.addr()
	dead.l.Close()
	_, err := cellularProbe(context.Background(), addr, tokenA, defaultProbeHost)
	var pe *phoneCheckError
	if !errors.As(err, &pe) || pe.Stage != stageConnect {
		t.Fatalf("unreachable phone: %v", err)
	}
	if _, err := cellularProbe(context.Background(), "8.8.8.8:80", tokenA, defaultProbeHost); err == nil {
		t.Fatal("the probe must only talk to the loopback tunnel")
	}
}

func TestRejectionIsTypedNotTextMatched(t *testing.T) {
	if asRejection(&phoneCheckError{Stage: stageUSBLoop, Status: 407, Err: errors.New("any wording")}) != errPhoneRejected {
		t.Fatal("a typed 407 must be a rejection whatever the message says")
	}
	if asRejection(errors.New("phone diagnostic HTTP 407 (check foreground app and current token)")) == errPhoneRejected {
		t.Fatal("message text alone must not classify a rejection")
	}
}

func TestProbeHostMustBeAllowlisted(t *testing.T) {
	for _, tc := range []struct {
		host    string
		allowed []string
		ok      bool
	}{
		{defaultProbeHost, nil, true},
		{"CDN.SteamContent.com", nil, true},
		{"evil.example.com", nil, false},
		{"notsteamcontent.com", nil, false},
		{"1.2.3.4", nil, false},
		{defaultProbeHost + ":443", nil, false},
		{defaultProbeHost + "/x", nil, false},
		{"", nil, false},
		{defaultProbeHost, []string{"example.org"}, false},
		{"a.example.org", []string{"example.org"}, true},
	} {
		if _, err := checkProbeHost(tc.host, tc.allowed); (err == nil) != tc.ok {
			t.Errorf("%q allowed=%v: err=%v", tc.host, tc.allowed, err)
		}
	}
}

// TestRunMainHelper turns the test binary into the accelerator for the subprocess tests.
func TestRunMainHelper(t *testing.T) {
	if os.Getenv("TF_R12_MAIN") != "1" {
		t.Skip("subprocess helper")
	}
	os.Args = append([]string{"steam-accelerator"}, strings.Split(os.Getenv("TF_R12_ARGS"), "\x1f")...)
	main()
}

func privateHomeIP(t *testing.T) string {
	t.Helper()
	addrs, _ := activeIPv4()
	for _, a := range addrs {
		if a.IP.IsPrivate() {
			return a.IP.String()
		}
	}
	t.Skip("no RFC1918 IPv4 interface on this machine; validateHome would stop the subprocess first")
	return ""
}

// The preflight must end startup before port 80 is bound. A listener already holds port
// 80 on the home address (or the OS refuses it), so a build that bound first would report
// "cannot bind"; one that probes first reports the probe failure.
func TestPreflightCellularFailureStopsBeforeBind(t *testing.T) {
	home := privateHomeIP(t)
	hold, err := net.Listen("tcp4", net.JoinHostPort(home, "80"))
	if err == nil {
		t.Cleanup(func() { hold.Close() })
	} else if !errors.Is(err, os.ErrPermission) && !strings.Contains(err.Error(), "in use") {
		t.Skipf("cannot make port 80 unbindable here: %v", err)
	}
	run := func(t *testing.T, p *r12Phone) string {
		dir := t.TempDir()
		args := []string{"--uplink-mode", "phone", "--external-tunnel", "--phone-proxy", p.addr(), "--home-ip", home,
			"--report", dir + "/report.json", "--recovery-log", dir + "/recovery.log"}
		cmd := exec.Command(os.Args[0], "-test.run=^TestRunMainHelper$")
		cmd.Env = append(os.Environ(), "TF_R12_MAIN=1", "TF_R12_ARGS="+strings.Join(args, "\x1f"))
		cmd.Stdin = strings.NewReader(tokenA + "\n")
		timer := time.AfterFunc(40*time.Second, func() { cmd.Process.Kill() })
		defer timer.Stop()
		out, runErr := cmd.CombinedOutput()
		if runErr == nil {
			t.Fatalf("startup must fail here:\n%s", out)
		}
		if strings.Contains(string(out), tokenA) {
			t.Fatalf("output leaks the token:\n%s", out)
		}
		return string(out)
	}
	connectLine := "connect " + defaultProbeHost + ":443"
	t.Run("CONNECT refused", func(t *testing.T) {
		p := newR12Phone(t, 2)
		p.connectStatus = 403
		out := run(t, p)
		if !strings.Contains(out, "cellular reachability preflight failed; no settings changed") || strings.Contains(out, "cannot bind") {
			t.Fatalf("a CONNECT failure must end startup before binding:\n%s", out)
		}
		if want := "info | download | " + connectLine; p.seen() != want {
			t.Fatalf("order = %s, want %s", p.seen(), want)
		}
	})
	t.Run("old protocol", func(t *testing.T) {
		p := newR12Phone(t, 1)
		out := run(t, p)
		if !strings.Contains(out, "update the iOS app") || strings.Contains(out, "cannot bind") || p.seen() != "info" {
			t.Fatalf("old protocol must stop first (%s):\n%s", p.seen(), out)
		}
	})
	t.Run("TLS failure continues to the bind", func(t *testing.T) {
		p := newR12Phone(t, 2) // CONNECT 200, then no TLS: warning only
		out := run(t, p)
		if !strings.Contains(out, "TLS verification failed") || !strings.Contains(out, "cannot bind") {
			t.Fatalf("a TLS failure is a warning and startup proceeds to the ports:\n%s", out)
		}
		if want := "info | download | " + connectLine; p.seen() != want {
			t.Fatalf("order = %s, want %s", p.seen(), want)
		}
	})
}
