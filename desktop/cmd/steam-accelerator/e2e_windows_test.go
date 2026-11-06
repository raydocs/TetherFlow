//go:build windows

package main

// Windows end-to-end tests (R14). They run the real executable, built with -trimpath into a
// directory whose name has spaces and CJK characters, against
//
//   - a fake Apple USB service on the production endpoint 127.0.0.1:27015, and
//   - a Go fake phone that speaks the real wire protocol 2 (/diagnostic/info, the exact-byte
//     /diagnostic/download, authenticated CONNECT, then a plain HTTP origin or a TLS server
//     whose certificate no system root signs),
//
// with the runner's real hosts file and the real ports 80 and 443 on its RFC1918 address.
// No internet origin is needed: --uplink-mode phone never touches the home path.
//
// SAFETY. These tests change the machine's hosts file and bind global ports. They run only
// when TF_E2E_WINDOWS=1, and then only on GitHub Actions (GITHUB_ACTIONS=true); anywhere else
// they fail loudly instead of touching anything. One process owns the ports, the tests are
// serialised, hosts is snapshotted first, and restoration removes only the TetherFlow blocks
// and the single foreign line this file appended. It never writes the snapshot back over the
// file, so a change by anyone else is left in place and reported.
//
// The fakes between the BEGIN/END markers are portable. Their self-tests run everywhere
// (Windows CI included, no opt-in) and are the part that can be run on a developer Mac.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"tetherflow-desktop/internal/steamsetup"
	"tetherflow-desktop/internal/usbmux"
)

// ===== BEGIN-PORTABLE-FAKES =====

const (
	e2ePayloadSize = 6 << 20
	// Crockford base32, 32 characters: the iOS app's token format.
	e2eTokenA     = "0123456789abcdefghjkmnpqrstvwxyz"
	e2eTokenB     = "zyxwvtsrqpnmkjhgfedcba9876543210"
	e2eSerial     = "00008110-001A2B3C4D5E6F78"
	e2eOriginHost = "cache1.steamcontent.com"
	e2eProbeHost  = "steampipe.akamaized.net"
)

// e2ePattern is the deterministic origin payload: 32-byte block i is sha256(seed || i).
type e2ePattern struct {
	seed   string
	n, off int64
}

func (p *e2ePattern) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	if int64(len(b)) > p.n-p.off {
		b = b[:p.n-p.off]
	}
	var block [32]byte
	cur := int64(-1)
	for i := range b {
		pos := p.off + int64(i)
		if idx := pos / 32; idx != cur {
			var ib [8]byte
			binary.BigEndian.PutUint64(ib[:], uint64(idx))
			h := sha256.New()
			h.Write([]byte(p.seed))
			h.Write(ib[:])
			copy(block[:], h.Sum(nil))
			cur = idx
		}
		b[i] = block[pos%32]
	}
	p.off += int64(len(b))
	return len(b), nil
}

// e2eVerify reads r to its end and requires exactly the n pattern bytes of seed.
func e2eVerify(r io.Reader, seed string, n int64) error {
	want := &e2ePattern{seed: seed, n: n}
	got := make([]byte, 64<<10)
	exp := make([]byte, 64<<10)
	var total int64
	for {
		m, err := r.Read(got)
		if m > 0 {
			k, _ := io.ReadFull(want, exp[:m])
			if k != m || !bytes.Equal(got[:m], exp[:m]) {
				return fmt.Errorf("payload differs from the expected bytes near offset %d", total)
			}
			total += int64(m)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("after %d bytes: %w", total, err)
		}
	}
	if total != n {
		return fmt.Errorf("received %d bytes, want exactly %d", total, n)
	}
	return nil
}

// e2eLeak returns a 16-character window of any token found in text, in any case and with
// dashes or spaces removed, or "" if there is none.
func e2eLeak(text string, tokens ...string) string {
	flat := strings.NewReplacer("-", "", " ", "").Replace(strings.ToLower(text))
	for _, tok := range tokens {
		for i := 0; i+16 <= len(tok); i += 4 {
			if w := tok[i : i+16]; strings.Contains(flat, w) {
				return w
			}
		}
	}
	return ""
}

// e2eLog is a goroutine-safe output buffer that tests can wait on.
type e2eLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *e2eLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *e2eLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
func (l *e2eLog) contains(s string) bool { return strings.Contains(l.String(), s) }

func closeWriteOf(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// ---- fake Apple USB service (usbmuxd plist protocol) ----

const e2ePlistHead = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
	`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n"

var (
	e2eDeviceIDRe = regexp.MustCompile(`<key>DeviceID</key>\s*<integer>(\d+)</integer>`)
	e2ePortRe     = regexp.MustCompile(`<key>PortNumber</key>\s*<integer>(\d+)</integer>`)
)

type e2eMux struct {
	ln      net.Listener
	phone   string
	serial  string
	wg      sync.WaitGroup
	mu      sync.Mutex
	id      uint32
	present bool
	closed  bool
	conns   map[net.Conn]struct{}
	okByID  map[uint32]int
	bad     int
	ports   []uint16
}

// newE2EMux listens on addr and bridges every accepted Connect to the phone listener.
func newE2EMux(addr, serial string, id uint32, phoneAddr string) (*e2eMux, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	m := &e2eMux{ln: ln, phone: phoneAddr, serial: serial, id: id, present: true, conns: map[net.Conn]struct{}{}, okByID: map[uint32]int{}}
	m.wg.Add(1)
	go m.serve()
	return m, nil
}

func (m *e2eMux) addr() string { return m.ln.Addr().String() }

func (m *e2eMux) track(c net.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.conns[c] = struct{}{}
	return true
}
func (m *e2eMux) untrack(c net.Conn) {
	c.Close()
	m.mu.Lock()
	delete(m.conns, c)
	m.mu.Unlock()
}

// Close stops the service and every bridged connection, and waits until the port is free.
func (m *e2eMux) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.ln.Close()
	for c := range m.conns {
		c.Close()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// SetDevice changes how the one phone is enumerated: a new DeviceID for the same serial, or
// absent. Connect to any other DeviceID is refused, as a real service does after a replug.
func (m *e2eMux) SetDevice(id uint32, present bool) {
	m.mu.Lock()
	m.id, m.present = id, present
	m.mu.Unlock()
}

// DropBridges closes every bridged connection, like unplugging the cable.
func (m *e2eMux) DropBridges() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for c := range m.conns {
		c.Close()
	}
}
func (m *e2eMux) connectsFor(id uint32) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.okByID[id]
}
func (m *e2eMux) badConnects() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bad
}
func (m *e2eMux) lastPort() uint16 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ports) == 0 {
		return 0
	}
	return m.ports[len(m.ports)-1]
}

func (m *e2eMux) serve() {
	defer m.wg.Done()
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		if !m.track(c) {
			c.Close()
			return
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer m.untrack(c)
			m.handle(c)
		}()
	}
}

func e2eResultPlist(n int) string {
	return e2ePlistHead + `<plist version="1.0"><dict><key>MessageType</key><string>Result</string><key>Number</key><integer>` + strconv.Itoa(n) + `</integer></dict></plist>`
}

func (m *e2eMux) deviceList() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := ""
	if m.present {
		entries = fmt.Sprintf(`<dict><key>DeviceID</key><integer>%d</integer><key>MessageType</key><string>Attached</string><key>Properties</key><dict><key>ConnectionSpeed</key><integer>480000000</integer><key>ConnectionType</key><string>USB</string><key>DeviceID</key><integer>%d</integer><key>SerialNumber</key><string>%s</string></dict></dict>`, m.id, m.id, m.serial)
	}
	return e2ePlistHead + `<plist version="1.0"><dict><key>DeviceList</key><array>` + entries + `</array></dict></plist>`
}

func (m *e2eMux) reply(c net.Conn, tag uint32, body string) error {
	p := make([]byte, 16+len(body))
	binary.LittleEndian.PutUint32(p[0:], uint32(len(p)))
	binary.LittleEndian.PutUint32(p[4:], 1)
	binary.LittleEndian.PutUint32(p[8:], 8)
	binary.LittleEndian.PutUint32(p[12:], tag)
	copy(p[16:], body)
	_, err := c.Write(p)
	return err
}

func (m *e2eMux) handle(c net.Conn) {
	br := bufio.NewReader(c)
	for {
		var h [16]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return
		}
		n := binary.LittleEndian.Uint32(h[0:4])
		if n < 16 || n > 1<<20 {
			return
		}
		body := make([]byte, n-16)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		tag := binary.LittleEndian.Uint32(h[12:16])
		text := string(body)
		switch {
		case strings.Contains(text, "<string>ListDevices</string>"):
			if m.reply(c, tag, m.deviceList()) != nil {
				return
			}
		case strings.Contains(text, "<string>Connect</string>"):
			m.connect(c, br, tag, text)
			return
		default:
			if m.reply(c, tag, e2eResultPlist(1)) != nil {
				return
			}
		}
	}
}

func (m *e2eMux) connect(c net.Conn, br *bufio.Reader, tag uint32, text string) {
	idm, pm := e2eDeviceIDRe.FindStringSubmatch(text), e2ePortRe.FindStringSubmatch(text)
	if idm == nil || pm == nil {
		_ = m.reply(c, tag, e2eResultPlist(1))
		return
	}
	id, _ := strconv.ParseUint(idm[1], 10, 32)
	raw, _ := strconv.ParseUint(pm[1], 10, 16)
	port := uint16(raw&255)<<8 | uint16(raw>>8) // the plist carries network byte order
	m.mu.Lock()
	ok := m.present && uint32(id) == m.id
	if ok {
		m.okByID[uint32(id)]++
		m.ports = append(m.ports, port)
	} else {
		m.bad++
	}
	m.mu.Unlock()
	if !ok {
		_ = m.reply(c, tag, e2eResultPlist(2)) // BadDevice
		return
	}
	up, err := net.DialTimeout("tcp", m.phone, 5*time.Second)
	if err != nil {
		_ = m.reply(c, tag, e2eResultPlist(3)) // connection refused
		return
	}
	if !m.track(up) {
		up.Close()
		return
	}
	defer m.untrack(up)
	if m.reply(c, tag, e2eResultPlist(0)) != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); closeWriteOf(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); closeWriteOf(c); done <- struct{}{} }()
	<-done
	<-done
}

// ---- fake phone: wire protocol 2 over one TCP listener ----

type e2eTunnelReq struct{ Target, Host, Path string }

type e2ePhone struct {
	ln      net.Listener
	cert    tls.Certificate
	wg      sync.WaitGroup
	mu      sync.Mutex
	token   string
	closed  bool
	conns   map[net.Conn]struct{}
	tunnels map[net.Conn]struct{}
	info    int
	loop    int
	c200    map[string]int
	c407    int
	tlsHit  int
	tlsFail int
	reqs    []e2eTunnelReq
	served  int64
}

func newE2EPhone(token string) (*e2ePhone, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: e2eProbeHost}, DNSNames: []string{e2eProbeHost},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &e2ePhone{ln: ln, token: token, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		conns: map[net.Conn]struct{}{}, tunnels: map[net.Conn]struct{}{}, c200: map[string]int{}}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

func (p *e2ePhone) addr() string { return p.ln.Addr().String() }

func (p *e2ePhone) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.ln.Close()
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

// SetToken rotates the token, as the iOS app does, and drops open tunnels so the next
// request must authenticate again.
func (p *e2ePhone) SetToken(tok string) {
	p.mu.Lock()
	p.token = tok
	for c := range p.tunnels {
		c.Close()
	}
	p.mu.Unlock()
}

type e2ePhoneStats struct {
	Info, Loop, Reject407, TLSAttempts, TLSFailures int
	Connect200                                      map[string]int
	Reqs                                            []e2eTunnelReq
	Served                                          int64
}

func (p *e2ePhone) stats() e2ePhoneStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := e2ePhoneStats{Info: p.info, Loop: p.loop, Reject407: p.c407, TLSAttempts: p.tlsHit, TLSFailures: p.tlsFail, Served: p.served,
		Connect200: map[string]int{}, Reqs: append([]e2eTunnelReq(nil), p.reqs...)}
	for k, v := range p.c200 {
		s.Connect200[k] = v
	}
	return s
}

// waitStats polls until ok(stats) holds: the fake records some events (a failed TLS
// handshake) just after the client has already moved on.
func (p *e2ePhone) waitStats(ok func(e2ePhoneStats) bool, d time.Duration) e2ePhoneStats {
	deadline := time.Now().Add(d)
	for {
		st := p.stats()
		if ok(st) || time.Now().After(deadline) {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *e2ePhone) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			c.Close()
			return
		}
		p.conns[c] = struct{}{}
		p.mu.Unlock()
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(c)
			c.Close()
			p.mu.Lock()
			delete(p.conns, c)
			delete(p.tunnels, c)
			p.mu.Unlock()
		}()
	}
}

func (p *e2ePhone) authorised(req *http.Request) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return req.Header.Get("Proxy-Authorization") == "Bearer "+p.token
}

type e2eBufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *e2eBufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (p *e2ePhone) handle(c net.Conn) {
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if !p.authorised(req) {
			p.mu.Lock()
			if req.Method == http.MethodConnect {
				p.c407++
			}
			p.mu.Unlock()
			io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		switch {
		case req.Method == http.MethodConnect:
			p.tunnel(c, br, req)
			return
		case req.URL.Path == "/diagnostic/info":
			p.mu.Lock()
			p.info++
			p.mu.Unlock()
			body := `{"protocol":2}`
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		case req.URL.Path == "/diagnostic/download":
			n, _ := strconv.Atoi(req.URL.Query().Get("bytes"))
			if n < 1 || n > 256<<20 {
				io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
				return
			}
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", n)
			buf := make([]byte, 64<<10)
			for off := 0; off < n; {
				k := len(buf)
				if n-off < k {
					k = n - off
				}
				for i := 0; i < k; i++ {
					buf[i] = byte((off + i) % 251) // the diagnostic's fixed pattern
				}
				if _, err := c.Write(buf[:k]); err != nil {
					return
				}
				off += k
			}
			p.mu.Lock()
			p.loop++
			p.mu.Unlock()
		default:
			io.WriteString(c, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n")
			return
		}
	}
}

func (p *e2ePhone) tunnel(c net.Conn, br *bufio.Reader, req *http.Request) {
	target := req.URL.Host
	if target == "" {
		target = req.Host
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		return
	}
	switch port {
	case "80":
		p.mu.Lock()
		p.tunnels[c] = struct{}{}
		p.c200[target]++
		p.mu.Unlock()
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		p.origin(c, br, target)
	case "443":
		p.mu.Lock()
		p.c200[target]++
		p.tlsHit++
		p.mu.Unlock()
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		conn := tls.Server(&e2eBufConn{Conn: c, r: br}, &tls.Config{Certificates: []tls.Certificate{p.cert}, MinVersion: tls.VersionTLS12})
		if err := conn.Handshake(); err != nil {
			p.mu.Lock()
			p.tlsFail++
			p.mu.Unlock()
		}
	default:
		io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
	}
}

// origin answers plain HTTP inside an opened CONNECT tunnel until it is closed.
func (p *e2ePhone) origin(c net.Conn, br *bufio.Reader, target string) {
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.Method != http.MethodGet {
			io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		p.mu.Lock()
		p.reqs = append(p.reqs, e2eTunnelReq{Target: target, Host: req.Host, Path: req.URL.Path})
		p.mu.Unlock()
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n", e2ePayloadSize)
		pat := &e2ePattern{seed: req.URL.Path, n: e2ePayloadSize}
		buf := make([]byte, 64<<10)
		for {
			k, rerr := pat.Read(buf)
			if k > 0 {
				w, werr := c.Write(buf[:k])
				p.mu.Lock()
				p.served += int64(w)
				p.mu.Unlock()
				if werr != nil {
					return
				}
			}
			if rerr != nil {
				break
			}
		}
		if req.Close {
			return
		}
	}
}

// ---- self-tests of the fakes against the production client code (no opt-in) ----

func TestE2EHelpersPatternVerify(t *testing.T) {
	var b bytes.Buffer
	if _, err := io.Copy(&b, &e2ePattern{seed: "/a", n: 100_003}); err != nil {
		t.Fatal(err)
	}
	if err := e2eVerify(bytes.NewReader(b.Bytes()), "/a", 100_003); err != nil {
		t.Fatalf("exact stream rejected: %v", err)
	}
	flipped := append([]byte(nil), b.Bytes()...)
	flipped[70_000] ^= 1
	for name, c := range map[string]struct {
		data []byte
		seed string
	}{"flipped bit": {flipped, "/a"}, "short": {b.Bytes()[:100_002], "/a"}, "long": {append(append([]byte(nil), b.Bytes()...), 0), "/a"}, "other seed": {b.Bytes(), "/b"}} {
		if e2eVerify(bytes.NewReader(c.data), c.seed, 100_003) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if e2eLeak("x "+strings.ToUpper(e2eTokenA[:8])+"-"+strings.ToUpper(e2eTokenA[8:]), e2eTokenA) == "" || e2eLeak("harmless", e2eTokenA, e2eTokenB) != "" {
		t.Fatal("leak detector is broken")
	}
}

func TestE2EHelpersFakesSpeakTheProductionProtocol(t *testing.T) {
	phone, err := newE2EPhone(e2eTokenA)
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	mux, err := newE2EMux("127.0.0.1:0", e2eSerial, 4, phone.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	client, err := usbmux.NewClient(usbmux.Config{Network: "tcp", Address: mux.addr(), HandshakeTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	prev := testUSBClient
	testUSBClient = client
	defer func() { testUSBClient = prev }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	addr, stop, err := startPhoneTunnel(ctx, "")
	if err != nil {
		t.Fatalf("production tunnel over the fake service: %v", err)
	}
	defer stop()
	// Production preflight: protocol 2, exact-byte loop, then CONNECT and TLS with system roots.
	if err := probeStartup(ctx, addr, e2eTokenA); err != nil {
		t.Fatalf("probeStartup: %v", err)
	}
	if err := probeStartup(ctx, addr, e2eTokenB); !errors.Is(err, errPhoneRejected) {
		t.Fatalf("wrong token: %v", err)
	}
	tlsWarn, err := cellularProbe(ctx, addr, e2eTokenA, e2eProbeHost)
	if err != nil {
		t.Fatalf("CONNECT: %v", err)
	}
	if tlsWarn == nil {
		t.Fatal("TLS to the untrusted fake certificate verified: the controlled failure is not controlled")
	}
	if mux.lastPort() != 8283 {
		t.Fatalf("forwarded port %d, want 8283 in network byte order", mux.lastPort())
	}
	st := phone.waitStats(func(s e2ePhoneStats) bool { return s.TLSFailures >= 1 }, 5*time.Second)
	if st.Info < 1 || st.Loop < 1 || st.Connect200[e2eProbeHost+":443"] != 1 || st.TLSFailures != 1 || st.Reject407 != 0 {
		t.Fatalf("phone stats %+v", st)
	}

	// An origin request through an authenticated CONNECT, exact bytes.
	fetch := func(path string) error {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		fmt.Fprintf(c, "CONNECT %s:80 HTTP/1.1\r\nHost: %s:80\r\nProxy-Authorization: Bearer %s\r\n\r\n", e2eOriginHost, e2eOriginHost, e2eTokenA)
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil || resp.StatusCode != 200 {
			return fmt.Errorf("CONNECT: %v %v", resp, err)
		}
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, e2eOriginHost)
		resp, err = http.ReadResponse(br, &http.Request{Method: http.MethodGet})
		if err != nil || resp.StatusCode != 200 {
			return fmt.Errorf("GET: %v %v", resp, err)
		}
		return e2eVerify(resp.Body, path, e2ePayloadSize)
	}
	if err := fetch("/depot/1/chunk/a"); err != nil {
		t.Fatal(err)
	}

	// Re-enumeration: the same serial under a new DeviceID. The forwarder lists on every
	// connection, so it must follow; nothing may connect to the old ID again.
	oldBefore := mux.connectsFor(4)
	mux.SetDevice(9, true)
	mux.DropBridges()
	if err := fetch("/depot/1/chunk/b"); err != nil {
		t.Fatalf("after re-enumeration: %v", err)
	}
	if mux.connectsFor(9) < 1 || mux.connectsFor(4) != oldBefore {
		t.Fatalf("connects: id4 %d (was %d), id9 %d", mux.connectsFor(4), oldBefore, mux.connectsFor(9))
	}
	old := usbmux.Device{DeviceID: 4, Serial: strings.ToLower(strings.ReplaceAll(e2eSerial, "-", "")), ConnectionType: "USB"}
	if conn, err := client.Connect(ctx, old, usbmux.ForwardPort); err == nil {
		conn.Close()
		t.Fatal("the fake accepted a Connect to a DeviceID that is gone")
	}
	// A token rotation: the old token gets 407 on CONNECT.
	phone.SetToken(e2eTokenB)
	if err := probeStartup(ctx, addr, e2eTokenA); !errors.Is(err, errPhoneRejected) {
		t.Fatalf("rotated token still accepted: %v", err)
	}
	if err := probeStartup(ctx, addr, e2eTokenB); err != nil {
		t.Fatalf("new token: %v", err)
	}
}

// ===== END-PORTABLE-FAKES =====

// ---- Windows harness ----

const e2eCJKDir = "tf e2e \u6d4b\u8bd5 \u76ee\u5f55 " // spaces and CJK in every path the exe runs from

var (
	e2eMu        sync.Mutex // one E2E test at a time in this process
	e2eKernel32  = syscall.NewLazyDLL("kernel32.dll")
	e2eCtrlEvent = e2eKernel32.NewProc("GenerateConsoleCtrlEvent")
	e2eAlloc     = e2eKernel32.NewProc("AllocConsole")
	e2eConsole   sync.Once
	e2eBuilt     struct {
		once sync.Once
		exe  string
		err  error
	}
)

// e2eGate: skip unless opted in; once opted in, never skip, and never run off GitHub Actions.
func e2eGate(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_E2E_WINDOWS") != "1" {
		t.Skip("opt-in Windows E2E: set TF_E2E_WINDOWS=1 (changes the real hosts file; uses ports 27015, 80, 443)")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("TF_E2E_WINDOWS=1 changes the real hosts file and global ports; refusing to run outside GitHub Actions (GITHUB_ACTIONS is not \"true\")")
	}
}

type e2eProcInfo struct {
	PID, PPID uint32
	Name      string
}

func e2eProcesses() ([]e2eProcInfo, error) {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(snap)
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := syscall.Process32First(snap, &pe); err != nil {
		return nil, err
	}
	var out []e2eProcInfo
	for {
		out = append(out, e2eProcInfo{PID: pe.ProcessID, PPID: pe.ParentProcessID, Name: syscall.UTF16ToString(pe.ExeFile[:])})
		if err := syscall.Process32Next(snap, &pe); err != nil {
			if errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
				return out, nil
			}
			return nil, err
		}
	}
}

func e2eKillPID(pid uint32) {
	if p, err := os.FindProcess(int(pid)); err == nil {
		_ = p.Kill()
	}
}

func e2eSDDL(path string) (string, error) {
	ps := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", "[System.IO.File]::GetAccessControl($env:TF_E2E_ACL_PATH).GetSecurityDescriptorSddlForm([System.Security.AccessControl.AccessControlSections]::All)")
	cmd.Env = append(os.Environ(), "TF_E2E_ACL_PATH="+path)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil || s == "" {
		return "", fmt.Errorf("read SDDL of %s: %v: %s", path, err, s)
	}
	return s, nil
}

func e2eBuildExe(t *testing.T) string {
	t.Helper()
	e2eBuilt.once.Do(func() {
		dir, err := os.MkdirTemp("", e2eCJKDir)
		if err != nil {
			e2eBuilt.err = err
			return
		}
		exe := filepath.Join(dir, "tf-e2e-steam-accelerator.exe")
		out, err := exec.Command("go", "build", "-trimpath", "-o", exe, ".").CombinedOutput()
		if err != nil {
			e2eBuilt.err = fmt.Errorf("go build -trimpath: %v\n%s", err, out)
			return
		}
		e2eBuilt.exe = exe
	})
	if e2eBuilt.err != nil {
		t.Fatalf("cannot build the real executable: %v", e2eBuilt.err)
	}
	return e2eBuilt.exe
}

type e2eEnv struct {
	t       *testing.T
	exe     string
	root    string
	homeIP  string
	hosts   string
	snap    []byte
	sddl    string
	started time.Time
	procs   []*e2eProc
	foreign []byte
}

func newE2E(t *testing.T) *e2eEnv {
	t.Helper()
	e2eGate(t)
	e2eMu.Lock()
	t.Cleanup(e2eMu.Unlock) // registered first, so it runs after the restoration below
	e := &e2eEnv{t: t, started: time.Now()}
	e.exe = e2eBuildExe(t)
	root, err := os.MkdirTemp(filepath.Dir(e.exe), "run-")
	if err != nil {
		t.Fatal(err)
	}
	e.root = root
	e.homeIP = e2eHomeIP(t)
	if e.hosts, err = hostsPath(); err != nil {
		t.Fatal(err)
	}
	in, err := steamsetup.Inspect(e.hosts)
	if err != nil {
		t.Fatalf("cannot inspect the runner's hosts file: %v", err)
	}
	if len(in.Leases) > 0 || in.Malformed || in.ForeignOverride || len(in.PendingJournals) > 0 {
		t.Fatalf("the runner's hosts file already holds TetherFlow or Steam-discovery state before the test: %+v", in)
	}
	if e.snap, err = os.ReadFile(e.hosts); err != nil {
		t.Fatal(err)
	}
	if e.sddl, err = e2eSDDL(e.hosts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.restore)
	e.waitPortsFree()
	return e
}

func e2eHomeIP(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("TF_E2E_HOME_IP"); v != "" {
		if err := validateHome(v); err != nil {
			t.Fatalf("TF_E2E_HOME_IP=%s: %v", v, err)
		}
		return v
	}
	addrs, err := activeIPv4()
	if err != nil {
		t.Fatalf("list runner interfaces: %v", err)
	}
	for _, a := range addrs {
		if ip4 := a.IP.To4(); ip4 != nil && ip4.IsPrivate() {
			return ip4.String()
		}
	}
	t.Fatalf("the runner has no active RFC1918 IPv4 address (found %d active IPv4 address(es)); the relay and hosts entry need one. This is a runner finding, not a skip. Set TF_E2E_HOME_IP only for an address that is assigned to an interface", len(addrs))
	return ""
}

// waitPortsFree proves this process can own the global ports; anything else holding them is
// reported by name instead of being worked around.
func (e *e2eEnv) waitPortsFree() {
	e.t.Helper()
	for _, addr := range []string{"127.0.0.1:27015", net.JoinHostPort(e.homeIP, "80"), net.JoinHostPort(e.homeIP, "443")} {
		deadline := time.Now().Add(20 * time.Second)
		for {
			l, err := net.Listen("tcp4", addr)
			if err == nil {
				l.Close()
				break
			}
			if time.Now().After(deadline) {
				e.t.Fatalf("%s is in use on the runner (%v); the E2E needs the production port. Reported as found (a runner service, Defender or IIS may hold it); not skipped", addr, err)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
}

type e2eProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	log   *e2eLog
	done  chan struct{}
	err   error
}

func e2eEnsureConsole() {
	e2eConsole.Do(func() { _, _, _ = e2eAlloc.Call() }) // fails harmlessly when this process already has a console
}

func (e *e2eEnv) start(args ...string) *e2eProc {
	e.t.Helper()
	e2eEnsureConsole()
	cmd := exec.Command(e.exe, args...)
	cmd.Dir = e.root
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP, needed for CTRL_BREAK
	log := &e2eLog{}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = 10 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start %s: %v", e.exe, err)
	}
	p := &e2eProc{cmd: cmd, stdin: in, log: log, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	e.procs = append(e.procs, p)
	return p
}

func (p *e2eProc) pid() uint32 { return uint32(p.cmd.Process.Pid) }
func (p *e2eProc) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
func (p *e2eProc) wait(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}
func (p *e2eProc) exitCode() int { return p.cmd.ProcessState.ExitCode() }
func (p *e2eProc) kill() {
	if p.running() {
		_ = p.cmd.Process.Kill()
		p.wait(10 * time.Second)
	}
}
func (p *e2eProc) sendLine(s string) { _, _ = io.WriteString(p.stdin, s+"\n") }
func (p *e2eProc) ctrlBreak() error {
	r, _, err := e2eCtrlEvent.Call(1, uintptr(p.cmd.Process.Pid)) // CTRL_BREAK_EVENT to the child's process group
	if r == 0 {
		return fmt.Errorf("GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT): %v", err)
	}
	return nil
}

// waitLog waits for text in the output; it fails fast if the process ends first.
func (p *e2eProc) waitLog(text string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p.log.contains(text) {
			return true
		}
		if !p.running() {
			return p.log.contains(text)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return p.log.contains(text)
}

func (e *e2eEnv) children(parent uint32) []e2eProcInfo {
	all, err := e2eProcesses()
	if err != nil {
		e.t.Fatalf("list processes: %v", err)
	}
	var out []e2eProcInfo
	for _, p := range all {
		if p.PPID == parent && strings.EqualFold(p.Name, filepath.Base(e.exe)) {
			out = append(out, p)
		}
	}
	return out
}
func e2ePIDAlive(pid uint32) bool {
	all, err := e2eProcesses()
	if err != nil {
		return true
	}
	for _, p := range all {
		if p.PID == pid {
			return true
		}
	}
	return false
}
func e2eWaitPIDGone(pid uint32, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !e2ePIDAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !e2ePIDAlive(pid)
}

// restore leaves the hosts file as found. It removes TetherFlow blocks through the production
// cleanup and the one foreign line this test appended; it never rewrites the file from the
// snapshot, so another party's change survives and is reported.
func (e *e2eEnv) restore() {
	t := e.t
	for _, p := range e.procs {
		p.kill()
	}
	deadline := time.Now().Add(45 * time.Second) // the guardians of killed sessions do this first
	for time.Now().Before(deadline) {
		if in, err := steamsetup.Inspect(e.hosts); err == nil && len(in.Leases) == 0 && len(in.PendingJournals) == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if in, err := steamsetup.Inspect(e.hosts); err == nil {
		for _, l := range in.Leases {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := cleanupHosts(ctx, e.hosts, l.Lease, 20*time.Second, flushDNS, ""); err != nil {
				t.Errorf("RESTORATION FAILED for lease %s: %v (run --recover-lease %s)", l.Lease, err, l.Lease)
			}
			cancel()
		}
	}
	if all, err := e2eProcesses(); err == nil {
		for _, p := range all {
			if strings.EqualFold(p.Name, filepath.Base(e.exe)) {
				e2eKillPID(p.PID) // a guardian that outlived its work
			}
		}
	}
	if len(e.foreign) > 0 {
		cur, err := os.ReadFile(e.hosts)
		if err == nil && bytes.Count(cur, e.foreign) == 1 {
			next := bytes.Replace(cur, e.foreign, nil, 1)
			if f, err := os.OpenFile(e.hosts, os.O_WRONLY, 0); err == nil {
				_, werr := f.WriteAt(next, 0)
				if werr == nil {
					werr = f.Truncate(int64(len(next)))
				}
				f.Close()
				if werr != nil {
					t.Errorf("could not remove the foreign test line from hosts: %v", werr)
				}
			} else {
				t.Errorf("could not open hosts to remove the foreign test line: %v", err)
			}
		} else {
			t.Errorf("the foreign test line is not in hosts exactly once (err=%v); left as found", err)
		}
	}
	if cur, err := os.ReadFile(e.hosts); err != nil {
		t.Errorf("read hosts after the test: %v", err)
	} else if !bytes.Equal(cur, e.snap) {
		t.Errorf("hosts differs from its pre-test snapshot after restoration (%s). It was NOT overwritten, so a change by someone else is preserved", e2eDiff(e.snap, cur))
	}
}

func e2eDiff(want, got []byte) string {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	return fmt.Sprintf("want %d bytes, got %d, first difference at offset %d", len(want), len(got), i)
}

// appendForeign adds a line that is not ours, as another tool would while a session runs.
func (e *e2eEnv) appendForeign() []byte {
	e.t.Helper()
	eol := "\n"
	if bytes.Contains(e.snap, []byte("\r\n")) {
		eol = "\r\n"
	}
	line := []byte("127.0.0.1 tf-e2e-foreign.invalid # added by another tool during the E2E" + eol)
	var err error
	for i := 0; i < 20; i++ {
		var f *os.File
		if f, err = os.OpenFile(e.hosts, os.O_WRONLY|os.O_APPEND, 0); err == nil {
			_, err = f.Write(line)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		e.t.Fatalf("append a foreign line to hosts: %v", err)
	}
	e.foreign = line
	return line
}

func (e *e2eEnv) assertHosts(want []byte, why string) {
	e.t.Helper()
	got, err := os.ReadFile(e.hosts)
	if err != nil {
		e.t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		e.t.Fatalf("%s: hosts is not byte-identical (%s)", why, e2eDiff(want, got))
	}
	sddl, err := e2eSDDL(e.hosts)
	if err != nil {
		e.t.Fatal(err)
	}
	if sddl != e.sddl {
		e.t.Fatalf("%s: hosts SDDL changed\nbefore: %s\nafter:  %s", why, e.sddl, sddl)
	}
}

func (e *e2eEnv) leases() []steamsetup.LeaseEntry {
	e.t.Helper()
	in, err := steamsetup.Inspect(e.hosts)
	if err != nil {
		e.t.Fatalf("inspect hosts: %v", err)
	}
	if in.Malformed {
		e.t.Fatal("hosts holds a malformed TetherFlow block")
	}
	return in.Leases
}
func (e *e2eEnv) waitNoLeases(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if in, err := steamsetup.Inspect(e.hosts); err == nil && len(in.Leases) == 0 {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// ---- fixture: fake USB service on the production port + fake phone + helpers ----

type e2eRig struct {
	*e2eEnv
	mux   *e2eMux
	phone *e2ePhone
}

func (e *e2eEnv) newRig() *e2eRig {
	e.t.Helper()
	phone, err := newE2EPhone(e2eTokenA)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(phone.Close)
	mux, err := newE2EMux("127.0.0.1:27015", e2eSerial, 4, phone.addr())
	if err != nil {
		e.t.Fatalf("the fake Apple USB service cannot bind the production endpoint 127.0.0.1:27015: %v", err)
	}
	e.t.Cleanup(mux.Close)
	return &e2eRig{e2eEnv: e, mux: mux, phone: phone}
}

func (e *e2eEnv) args(enable bool) []string {
	a := []string{"--uplink-mode", "phone", "--home-ip", e.homeIP,
		"--report", filepath.Join(e.root, "status.json"), "--recovery-log", filepath.Join(e.root, "recovery.log")}
	if enable {
		a = append(a, "--enable-steam")
	}
	return a
}

var e2eLeaseRe = regexp.MustCompile(`recovery lease=([0-9a-f]{32})`)

// startAccel runs the real exe, types the token on its stdin and waits until it listens.
func (r *e2eRig) startAccel(enable bool) (*e2eProc, string) {
	r.t.Helper()
	r.waitPortsFreeExceptMux()
	p := r.start(r.args(enable)...)
	p.sendLine(e2eTokenA)
	if !p.waitLog("Listening ", 90*time.Second) {
		r.t.Fatalf("the accelerator did not reach its listening state (running=%v):\n%s", p.running(), p.log.String())
	}
	lease := ""
	if enable {
		m := e2eLeaseRe.FindStringSubmatch(p.log.String())
		if m == nil {
			r.t.Fatalf("no recovery lease in the output:\n%s", p.log.String())
		}
		lease = m[1]
	}
	return p, lease
}

func (r *e2eRig) waitPortsFreeExceptMux() {
	for _, port := range []string{"80", "443"} {
		addr := net.JoinHostPort(r.homeIP, port)
		deadline := time.Now().Add(20 * time.Second)
		for {
			l, err := net.Listen("tcp4", addr)
			if err == nil {
				l.Close()
				break
			}
			if time.Now().After(deadline) {
				r.t.Fatalf("%s is still in use: %v", addr, err)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// get sends Host: cache1.steamcontent.com to HomeIP:80 and verifies the exact payload.
func (e *e2eEnv) get(path string) (int, error) {
	client := &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{
		Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(e.homeIP)}, Timeout: 5 * time.Second}
			return d.DialContext(ctx, "tcp4", net.JoinHostPort(e.homeIP, "80"))
		}}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, "http://"+e2eOriginHost+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, nil
	}
	if resp.ContentLength != e2ePayloadSize {
		return resp.StatusCode, fmt.Errorf("Content-Length %d, want %d", resp.ContentLength, e2ePayloadSize)
	}
	return resp.StatusCode, e2eVerify(resp.Body, path, e2ePayloadSize)
}

func (e *e2eEnv) mustGet(path string) {
	e.t.Helper()
	status, err := e.get(path)
	if status != http.StatusOK || err != nil {
		e.t.Fatalf("GET %s: status %d, err %v", path, status, err)
	}
}

// getEventually tolerates the relay's short cooldown after a failed phone dial.
func (e *e2eEnv) getEventually(path string, d time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(d)
	var status int
	var err error
	for time.Now().Before(deadline) {
		if status, err = e.get(path); status == http.StatusOK && err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.t.Fatalf("GET %s never succeeded within %v: last status %d, err %v", path, d, status, err)
}

type e2eReport struct {
	UplinkMode string `json:"uplink_mode"`
	Uplinks    struct {
		Home, Phone struct{ Requests, Bytes, Errors uint64 }
	} `json:"uplinks"`
	PhoneAuth *struct {
		Stale      bool
		Generation uint64
		Rejections uint64
	} `json:"phone_auth"`
}

func (e *e2eEnv) report() (e2eReport, error) {
	var r e2eReport
	data, err := os.ReadFile(filepath.Join(e.root, "status.json"))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(data, &r)
}

func (e *e2eEnv) waitReportPhoneBytes(want uint64, d time.Duration) e2eReport {
	e.t.Helper()
	deadline := time.Now().Add(d)
	var last e2eReport
	for time.Now().Before(deadline) {
		if r, err := e.report(); err == nil {
			last = r
			if r.Uplinks.Phone.Bytes >= want {
				return r
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.t.Fatalf("the status report never showed %d phone bytes (last: %+v)", want, last.Uplinks)
	return last
}

// stopClean ends the process the way a user does and returns its exit code.
func (e *e2eEnv) stopClean(p *e2eProc) int {
	e.t.Helper()
	if err := p.ctrlBreak(); err != nil {
		e.t.Fatalf("%v\noutput:\n%s", err, p.log.String())
	}
	if !p.wait(45 * time.Second) {
		e.t.Fatalf("the accelerator did not exit within 45s of CTRL_BREAK:\n%s", p.log.String())
	}
	return p.exitCode()
}

// noTokenAnywhere scans everything the run could have left: its output, files in its
// directory and new files directly in the temp directory.
func (e *e2eEnv) noTokenAnywhere(outputs ...string) {
	e.t.Helper()
	for i, o := range outputs {
		if w := e2eLeak(o, e2eTokenA, e2eTokenB); w != "" {
			e.t.Fatalf("output %d contains a token fragment %q", i, w)
		}
	}
	scan := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 8<<20 {
			return
		}
		if w := e2eLeak(string(data), e2eTokenA, e2eTokenB); w != "" {
			e.t.Fatalf("file %s contains a token fragment %q", path, w)
		}
	}
	_ = filepath.WalkDir(e.root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			scan(p)
		}
		return nil
	})
	if entries, err := os.ReadDir(os.TempDir()); err == nil {
		for _, ent := range entries {
			if info, err := ent.Info(); err == nil && !ent.IsDir() && info.ModTime().After(e.started) {
				scan(filepath.Join(os.TempDir(), ent.Name()))
			}
		}
	}
}

// ---- the six named end-to-end tests, plus the doctor assertion ----

func TestE2EPhoneModeDownloadExact(t *testing.T) {
	e := newE2E(t)
	r := e.newRig()
	p, lease := r.startAccel(true)
	out := p.log.String()
	for _, want := range []string{
		"Authenticated iPhone protocol-2 info and USB loop passed",
		"WARNING: the phone reached " + e2eProbeHost + ":443 but TLS verification failed", // controlled WARN, not a failure
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("startup output lacks %q:\n%s", want, out)
		}
	}
	if st := r.phone.waitStats(func(s e2ePhoneStats) bool { return s.TLSFailures >= 1 }, 10*time.Second); st.Info < 1 || st.Loop < 1 || st.Connect200[e2eProbeHost+":443"] < 1 || st.TLSFailures < 1 {
		t.Fatalf("the preflight did not run on the wire: %+v", st)
	}
	if ls := e.leases(); len(ls) != 1 || ls[0].Lease != lease || ls[0].IP != e.homeIP {
		t.Fatalf("hosts leases %+v, want the one session %s at %s", ls, lease, e.homeIP)
	}
	const path = "/depot/570/chunk/e2e-exact"
	e.mustGet(path)
	st := r.phone.stats()
	if len(st.Reqs) != 1 || st.Reqs[0].Target != e2eOriginHost+":80" || strings.TrimSuffix(st.Reqs[0].Host, ":80") != e2eOriginHost || st.Reqs[0].Path != path {
		t.Fatalf("the phone saw %+v, want one request for %s%s", st.Reqs, e2eOriginHost, path)
	}
	if st.Served != e2ePayloadSize {
		t.Fatalf("the phone served %d bytes, want %d", st.Served, e2ePayloadSize)
	}
	rep := e.waitReportPhoneBytes(e2ePayloadSize, 30*time.Second)
	if rep.Uplinks.Phone.Bytes != e2ePayloadSize || rep.Uplinks.Home.Bytes != 0 || rep.Uplinks.Home.Requests != 0 || rep.UplinkMode != "phone" {
		t.Fatalf("report %+v does not match the %d bytes served by the phone only", rep, e2ePayloadSize)
	}
	if code := e.stopClean(p); code != 0 {
		t.Fatalf("exit code %d:\n%s", code, p.log.String())
	}
	e.assertHosts(e.snap, "after a clean stop")
}

func TestE2EStaleTokenRefreshOverStdin(t *testing.T) {
	e := newE2E(t)
	r := e.newRig()
	p, _ := r.startAccel(false)
	e.mustGet("/depot/570/chunk/before-rotation")

	// The app rotates its token: open tunnels drop and CONNECT with the old token is a 407.
	r.phone.SetToken(e2eTokenB)
	status, _ := e.get("/depot/570/chunk/while-stale")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("while the token is stale the phone-only relay must refuse (503), got %d", status)
	}
	if !p.waitLog("PHONE TOKEN REJECTED", 30*time.Second) {
		t.Fatalf("no refresh prompt:\n%s", p.log.String())
	}
	if r.phone.stats().Reject407 < 1 {
		t.Fatal("the phone never answered 407")
	}
	p.sendLine(e2eTokenB) // typed on stdin, as the prompt asks
	if !p.waitLog("Phone token refreshed; the phone path is serving again.", 45*time.Second) {
		t.Fatalf("the new token was not accepted:\n%s", p.log.String())
	}
	e.getEventually("/depot/570/chunk/after-refresh", 30*time.Second)
	rep := e.waitReportPhoneBytes(2*e2ePayloadSize, 30*time.Second)
	if rep.PhoneAuth == nil || rep.PhoneAuth.Stale || rep.PhoneAuth.Generation < 1 || rep.Uplinks.Home.Bytes != 0 {
		t.Fatalf("report after refresh: %+v %+v", rep.PhoneAuth, rep.Uplinks)
	}
	if code := e.stopClean(p); code != 0 {
		t.Fatalf("exit code %d:\n%s", code, p.log.String())
	}
	recovery, _ := os.ReadFile(filepath.Join(e.root, "recovery.log"))
	e.noTokenAnywhere(p.log.String(), string(recovery))
}

func TestE2EReenumerationReconnects(t *testing.T) {
	e := newE2E(t)
	r := e.newRig()
	p, _ := r.startAccel(false)
	e.mustGet("/depot/570/chunk/before-replug")
	before4 := r.mux.connectsFor(4)
	if before4 < 1 {
		t.Fatal("the first download did not go through the fake USB service")
	}

	// Cable pulled: the phone is gone, bridges drop. A phone-only relay has nowhere else to go.
	r.mux.SetDevice(4, false)
	r.mux.DropBridges()
	if status, _ := e.get("/depot/570/chunk/while-unplugged"); status == http.StatusOK {
		t.Fatal("a download succeeded while the phone was unplugged")
	}
	// Replugged: the same serial, a different DeviceID.
	r.mux.SetDevice(9, true)
	e.getEventually("/depot/570/chunk/after-replug", 40*time.Second)
	if r.mux.connectsFor(9) < 1 {
		t.Fatalf("no Connect used the new DeviceID 9 (id 4: %d, id 9: %d)", r.mux.connectsFor(4), r.mux.connectsFor(9))
	}
	if r.mux.connectsFor(4) != before4 {
		t.Fatalf("a Connect still used the old DeviceID after re-enumeration: %d -> %d", before4, r.mux.connectsFor(4))
	}
	if code := e.stopClean(p); code != 0 {
		t.Fatalf("exit code %d:\n%s", code, p.log.String())
	}
}

func TestE2EForceKillGuardianCleansHosts(t *testing.T) {
	e := newE2E(t)
	r := e.newRig()
	p, lease := r.startAccel(true)
	if ls := e.leases(); len(ls) != 1 || ls[0].Lease != lease {
		t.Fatalf("leases %+v, want %s", ls, lease)
	}
	guardians := e.children(p.pid())
	if len(guardians) != 1 {
		t.Fatalf("expected exactly one guardian child of pid %d, found %+v", p.pid(), guardians)
	}
	foreign := e.appendForeign()
	p.kill() // TerminateProcess: no deferred cleanup runs
	if !e.waitNoLeases(60 * time.Second) {
		t.Fatalf("the guardian did not remove the session block within 60s; leases %+v", e.leases())
	}
	want := append(append([]byte(nil), e.snap...), foreign...)
	e.assertHosts(want, "after the guardian cleaned a killed session (the foreign line must survive)")
	if !e2eWaitPIDGone(guardians[0].PID, 30*time.Second) {
		t.Fatalf("the guardian (pid %d) did not exit after cleaning", guardians[0].PID)
	}
	if data, err := os.ReadFile(filepath.Join(e.root, "recovery.log")); err == nil && len(data) > 0 {
		t.Fatalf("a successful guardian cleanup wrote to the recovery log:\n%s", data)
	}
}

func TestE2ECtrlBreakCleanExit(t *testing.T) {
	e := newE2E(t)
	r := e.newRig()
	p, lease := r.startAccel(true)
	guardians := e.children(p.pid())
	if len(guardians) != 1 {
		t.Fatalf("expected one guardian, found %+v", guardians)
	}
	e.mustGet("/depot/570/chunk/before-stop")
	if code := e.stopClean(p); code != 0 {
		t.Fatalf("CTRL_BREAK exit code %d, want 0:\n%s", code, p.log.String())
	}
	for _, l := range e.leases() {
		t.Fatalf("lease %s is still in hosts after a clean exit", l.Lease)
	}
	e.assertHosts(e.snap, "after CTRL_BREAK (lease "+lease+")")
	if !e2eWaitPIDGone(guardians[0].PID, 30*time.Second) {
		t.Fatalf("the guardian (pid %d) outlived a clean exit", guardians[0].PID)
	}
	if data, err := os.ReadFile(filepath.Join(e.root, "recovery.log")); err == nil && len(data) > 0 {
		t.Fatalf("a clean exit wrote to the recovery log:\n%s", data)
	}
	if _, err := e.report(); err != nil {
		t.Fatalf("the final status report was not written: %v", err)
	}
}

func TestE2EStaleBlockReclaimedAfterGuardianKilled(t *testing.T) {
	e := newE2E(t)
	r := e.newRig()
	p1, stale := r.startAccel(true)
	guardians := e.children(p1.pid())
	if len(guardians) != 1 {
		t.Fatalf("expected one guardian, found %+v", guardians)
	}
	e2eKillPID(guardians[0].PID) // guardian first, so nobody is left to clean
	if !e2eWaitPIDGone(guardians[0].PID, 15*time.Second) {
		t.Fatal("could not kill the guardian")
	}
	p1.kill()
	ls := e.leases()
	if len(ls) != 1 || ls[0].Lease != stale || ls[0].IP != e.homeIP {
		t.Fatalf("the stale block should remain after both processes are killed: %+v", ls)
	}

	p2, fresh := r.startAccel(true)
	if fresh == stale {
		t.Fatal("the new session reused the stale lease")
	}
	if strings.Contains(p2.log.String(), "not provably dead") {
		t.Fatalf("the stale block was refused instead of reclaimed:\n%s", p2.log.String())
	}
	ls = e.leases()
	if len(ls) != 1 || ls[0].Lease != fresh {
		t.Fatalf("after reclaim hosts holds %+v, want only the new lease %s", ls, fresh)
	}
	e.mustGet("/depot/570/chunk/after-reclaim")
	if code := e.stopClean(p2); code != 0 {
		t.Fatalf("exit code %d:\n%s", code, p2.log.String())
	}
	e.assertHosts(e.snap, "after the reclaiming session stopped")
}

// The real executable's --doctor --json on the runner, with the fake USB service stopped
// first: the Apple USB service must be reported as FAIL and the exit status must be 2.
func TestE2EDoctorReportsUSBServiceFail(t *testing.T) {
	e := newE2E(t)
	r := e.newRig()
	r.mux.Close()
	e.waitPortsFree()
	cmd := exec.Command(e.exe, "--doctor", "--json", "--uplink-mode", "phone")
	cmd.Dir = e.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("doctor exit: %v (want status 2)\nstdout:\n%s\nstderr:\n%s", err, out, stderr.String())
	}
	var rep doctorReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("doctor --json is not JSON: %v\n%s", err, out)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Name == "apple-usb-service" {
			found = true
			if c.Status != statusFail {
				t.Fatalf("apple-usb-service is %s, want FAIL: %+v", c.Status, c)
			}
		}
	}
	if !found || rep.Result != statusFail {
		t.Fatalf("no apple-usb-service check or overall result %q:\n%s", rep.Result, out)
	}
	if w := e2eLeak(string(out)+stderr.String(), e2eTokenA, e2eTokenB); w != "" {
		t.Fatalf("doctor output contains token fragment %q", w)
	}
	e.assertHosts(e.snap, "doctor is read-only")
}
