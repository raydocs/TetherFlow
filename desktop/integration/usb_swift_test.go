//go:build darwin

package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
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

	"tetherflow-desktop/internal/steamaccel"
)

// Uses actual production Swift CONNECT/streaming code over loopback TCP, not a
// Go reimplementation of the phone. Only the test binary's egress is remapped.
func TestGoRelayThroughProductionSwiftCore(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "direct-loopback"
		if native {
			name = "native-usbmux"
		}
		t.Run(name, func(t *testing.T) { testGoRelayThroughSwift(t, native) })
	}
}
func testGoRelayThroughSwift(t *testing.T, native bool) {
	fixture := os.Getenv("TF_SWIFT_FIXTURE")
	if fixture == "" {
		t.Skip("run integration/verify-host.sh for the compiled Swift transport fixture")
	}
	payload := make([]byte, 2*1024*1024+137)
	if _, e := rand.Read(payload); e != nil {
		t.Fatal(e)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials leaked to origin")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(15 * time.Millisecond)
		_, _ = w.Write(payload)
	}))
	defer origin.Close()
	originTLS := httptest.NewTLSServer(origin.Config.Handler)
	defer originTLS.Close()
	_, tlsPortText, _ := net.SplitHostPort(strings.TrimPrefix(originTLS.URL, "https://"))
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	token := strings.Repeat("c", 64) // TEST-ONLY credential; never a real phone token.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, fixture, portText, tlsPortText, token)
	stdout, e := command.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	command.Stderr = os.Stderr
	if e = command.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cancel(); _ = command.Wait() }()
	ready := make(chan string, 1)
	go func() { s, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- s }()
	var phone string
	select {
	case line := <-ready:
		var p int
		if _, e = fmt.Sscanf(line, "READY %d", &p); e != nil || p <= 0 {
			t.Fatalf("fixture readiness %q %v", line, e)
		}
		phone = net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
	case <-time.After(8 * time.Second):
		t.Fatal("Swift listener startup timeout")
	}
	if native {
		phone = nativeMuxTunnel(t, phone)
	}
	cfg := steamaccel.Config{HomeIPv4: "127.0.0.1", AllowedHosts: []string{"steamcontent.com", "example.com"}, PhoneToken: token, PhoneProxyAddr: phone, LookupIP: func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }, HomeDialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
		target := strings.TrimPrefix(origin.URL, "http://")
		if strings.HasSuffix(address, ":443") {
			target = strings.TrimPrefix(originTLS.URL, "https://")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}}
	relay, e := steamaccel.New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer relay.Close()
	local := httptest.NewServer(relay)
	defer local.Close()
	client := &http.Client{Timeout: 15 * time.Second}
	get := func(base string) error {
		req, _ := http.NewRequest("GET", base+"/depot/fixture/chunk/integrity", nil)
		req.Host = "cache.example.steamcontent.com"
		resp, e := client.Do(req)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		body, e := io.ReadAll(resp.Body)
		if e != nil {
			return e
		}
		if resp.StatusCode != 200 || !bytes.Equal(body, payload) {
			return fmt.Errorf("corrupted/status%d: %d bytes", resp.StatusCode, len(body))
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := get(local.URL); e != nil {
				errs <- e
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	// Steam sends many chunks over persistent connections. Exercise real tunnel
	// reuse after the parallel burst, not just one HTTP response per CONNECT.
	for i := 0; i < 10; i++ {
		if err := get(local.URL); err != nil {
			t.Fatal(err)
		}
	}
	status := relay.Snapshot()
	if status.Home.Bytes == 0 || status.Phone.Bytes == 0 {
		t.Fatalf("both real transports not exercised: %+v", status)
	}
	if status.Home.Bytes+status.Phone.Bytes != uint64(len(payload)*30) {
		t.Fatalf("body accounting mismatch: %+v", status)
	}
	t.Logf("Go home + production Swift USB-core concurrent integrity: home=%d phone=%d bytes (host fixtures, NOT device speed)", status.Home.Bytes, status.Phone.Bytes)
	// Opaque TLS is checked end-to-end with the real origin certificate trusted
	// explicitly, never InsecureSkipVerify. Production Swift carries encrypted bytes.
	tlsListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tlsListener.Close()
	tlsServeDone := make(chan error, 1)
	go func() { tlsServeDone <- relay.ServeTLS(tlsListener) }()
	roots := x509.NewCertPool()
	roots.AddCert(originTLS.Certificate())
	tlsTransport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", tlsListener.Addr().String())
	}}
	defer tlsTransport.CloseIdleConnections()
	tlsClient := &http.Client{Transport: tlsTransport, Timeout: 15 * time.Second}
	tlsErrors := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := tlsClient.Get("https://example.com/depot/fixture/chunk/tls")
			if err != nil {
				tlsErrors <- err
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				tlsErrors <- err
				return
			}
			if resp.StatusCode != 200 || !bytes.Equal(body, payload) {
				tlsErrors <- fmt.Errorf("TLS payload corrupted/status%d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(tlsErrors)
	for err := range tlsErrors {
		t.Error(err)
	}
	tlsStatus := relay.Snapshot()
	if tlsStatus.Home.Requests <= status.Home.Requests || tlsStatus.Phone.Requests <= status.Phone.Requests {
		t.Fatalf("TLS did not exercise both paths: before=%+v after=%+v", status, tlsStatus)
	}
	_ = tlsListener.Close()
	select {
	case <-tlsServeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("TLS accept loop leaked")
	}
	// A new relay prefers the weighted phone path; a killed Swift peer must fail
	// before response commitment and retry the exact download through home.
	cancel()
	_ = command.Wait()
	fallback, e := steamaccel.New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer fallback.Close()
	fallbackLocal := httptest.NewServer(fallback)
	defer fallbackLocal.Close()
	if e = get(fallbackLocal.URL); e != nil {
		t.Fatal(e)
	}
	state := fallback.Snapshot()
	if state.Phone.Errors == 0 || state.Home.Bytes != uint64(len(payload)) {
		t.Fatalf("phone-disconnect failover missing: %+v", state)
	}
}
