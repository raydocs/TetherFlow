package steamaccel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const tlsTestHost = "cache.steamcontent.com"

func tlsTestCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: tlsTestHost}, DNSNames: []string{tlsTestHost}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func startTLSIngress(t *testing.T, r *Relay) (net.Listener, <-chan error) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.ServeTLS(l) }()
	t.Cleanup(func() {
		l.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ServeTLS did not exit")
		}
	})
	return l, done
}

// Real verified TLS origin and authenticated CONNECT443 proxy. Only trusted dial
// seams map validated public numeric targets to the local test origin.
func tlsIntegration(t *testing.T, c Config, handler http.Handler, configure func(*Config, string)) (*Relay, *http.Client, net.Listener, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	cert, roots := tlsTestCertificate(t)
	origin := httptest.NewUnstartedServer(handler)
	origin.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	origin.StartTLS()
	t.Cleanup(origin.Close)
	addr := origin.Listener.Addr().String()
	var home, phone atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "CONNECT" || req.Host != "8.8.8.8:443" || req.Header.Get("Proxy-Authorization") != "Bearer test-secret" {
			t.Error("invalid authenticated pinned CONNECT443")
			http.Error(w, "forbidden", 407)
			return
		}
		phone.Add(1)
		up, err := net.Dial("tcp", addr)
		if err != nil {
			http.Error(w, "origin", 502)
			return
		}
		defer up.Close()
		down, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer down.Close()
		if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if rw.Flush() != nil {
			return
		}
		done := make(chan struct{})
		go func() { io.Copy(up, rw); closeWrite(up); close(done) }()
		io.Copy(down, up)
		down.Close()
		up.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	c.PhoneProxyAddr = strings.TrimPrefix(proxy.URL, "http://")
	c.HomeDialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
		if network != "tcp4" || target != "8.8.8.8:443" {
			t.Errorf("unpinned home: %s %s", network, target)
		}
		home.Add(1)
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if configure != nil {
		configure(&c, addr)
	}
	r := newRelay(t, c)
	l, _ := startTLSIngress(t, r)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", l.Addr().String())
		if err != nil {
			return nil, err
		}
		return &fragmentHelloConn{Conn: conn}, nil
	}}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	t.Cleanup(func() { r.Close() })
	return r, client, l, &home, &phone
}

// Fragment the first ClientHello into TLS records, not merely TCP writes.
type fragmentHelloConn struct {
	net.Conn
	written bool
}

func (c *fragmentHelloConn) Write(p []byte) (int, error) {
	if c.written {
		return c.Conn.Write(p)
	}
	c.written = true
	wire := fragmentHello(p)
	_, err := writeHello(c.Conn, wire)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
func fragmentHello(wire []byte) []byte {
	var out []byte
	for len(wire) >= 5 {
		n := int(binary.BigEndian.Uint16(wire[3:5]))
		if n > len(wire)-5 {
			panic("invalid test TLS record")
		}
		payload := wire[5 : 5+n]
		for len(payload) > 0 {
			size := 13
			if size > len(payload) {
				size = len(payload)
			}
			out = append(out, wire[:3]...)
			out = append(out, byte(size>>8), byte(size))
			out = append(out, payload[:size]...)
			payload = payload[size:]
		}
		wire = wire[5+n:]
	}
	return append(out, wire...)
}

func TestTLSVerifiedParallelRangesBothPaths(t *testing.T) {
	const count = 12
	payload := bytes.Repeat([]byte{0, 0xff, 'x', 0x80}, 20000)
	entered := make(chan struct{}, count)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	c := testConfig()
	c.MaxPhoneConnections = 3
	r, client, _, home, phone := tlsIntegration(t, c, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Host != tlsTestHost || req.Header.Get("Range") != "bytes=4-80003" || req.Header.Get("If-Range") != `"tls-chunk"` {
			t.Error("TLS HTTP target/range changed")
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		w.Header().Set("Content-Range", "bytes 4-80003/90000")
		w.Header().Set("ETag", `"tls-chunk"`)
		w.WriteHeader(206)
		w.Write(payload)
	}), nil)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", "https://"+tlsTestHost+"/chunk?x=%2F", nil)
			req.Header.Set("Range", "bytes=4-80003")
			req.Header.Set("If-Range", `"tls-chunk"`)
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil || !bytes.Equal(got, payload) || resp.StatusCode != 206 || resp.Header.Get("ETag") != `"tls-chunk"` || len(resp.TLS.VerifiedChains) == 0 {
				t.Errorf("unverified or changed TLS range: %v", err)
			}
		}()
	}
	for i := 0; i < count; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("parallel TLS did not reach origin")
		}
	}
	if s := r.Snapshot(); s.Home.Active != 9 || s.Phone.Active != 3 || home.Load() == 0 || phone.Load() != 3 || phoneUsed(r) != 3 {
		t.Fatalf("dual path / budget not exercised: %+v home=%d phone=%d", s, home.Load(), phone.Load())
	}
	requireHealthyPhone(t, r)
	once.Do(func() { close(release) })
	wg.Wait()
	waitBudget(t, func() bool { s := r.Snapshot(); return s.Home.Active+s.Phone.Active == 0 && phoneUsed(r) == 0 })
	if s := r.Snapshot(); s.Home.Bytes+s.Phone.Bytes <= uint64(count*len(payload)) {
		t.Fatalf("missing encrypted wire byte counters: %+v", s)
	}
}

func TestTLSCertificateValidationNotBypassed(t *testing.T) {
	_, client, _, _, _ := tlsIntegration(t, testConfig(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }), nil)
	transport := client.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	_, err := client.Get("https://" + tlsTestHost + "/")
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected genuine certificate rejection, got %v", err)
	}
}

type helloMemoryConn struct {
	net.Conn
	bytes.Buffer
}

func (c *helloMemoryConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *helloMemoryConn) Write(p []byte) (int, error) { return c.Buffer.Write(p) }
func (c *helloMemoryConn) Close() error                { return nil }
func (c *helloMemoryConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}
func (c *helloMemoryConn) LocalAddr() net.Addr              { return c.RemoteAddr() }
func (c *helloMemoryConn) SetDeadline(time.Time) error      { return nil }
func (c *helloMemoryConn) SetReadDeadline(time.Time) error  { return nil }
func (c *helloMemoryConn) SetWriteDeadline(time.Time) error { return nil }
func testHello(t *testing.T, name string) []byte {
	t.Helper()
	c := new(helloMemoryConn)
	_ = tls.Client(c, &tls.Config{ServerName: name}).Handshake()
	if c.Len() == 0 {
		t.Fatal("no test ClientHello generated")
	}
	return append([]byte(nil), c.Bytes()...)
}

// Add an extension to a generated single-record ClientHello, adjusting every
// enclosing length. Large inputs are fragmented before being sent on the wire.
func helloWithExtension(t *testing.T, wire []byte, kind uint16, data []byte) []byte {
	t.Helper()
	out := append([]byte(nil), wire...)
	if len(out) < 44 || int(binary.BigEndian.Uint16(out[3:5])) != len(out)-5 {
		t.Fatal("test hello must be one record")
	}
	offset := 5 + 4 + 34
	for _, width := range []int{1, 2, 1} {
		n := int(out[offset])
		if width == 2 {
			n = int(binary.BigEndian.Uint16(out[offset:]))
		}
		offset += width + n
	}
	old := int(binary.BigEndian.Uint16(out[offset:]))
	out = append(out, byte(kind>>8), byte(kind), byte(len(data)>>8), byte(len(data)))
	out = append(out, data...)
	binary.BigEndian.PutUint16(out[offset:], uint16(old+4+len(data)))
	n := len(out) - 9
	out[6], out[7], out[8] = byte(n>>16), byte(n>>8), byte(n)
	binary.BigEndian.PutUint16(out[3:5], uint16(len(out)-5))
	return out
}

func TestTLSRejectsBeforeDialWithoutWritingAlerts(t *testing.T) {
	hello := testHello(t, tlsTestHost)
	oversized := []byte{22, 3, 3, 0, 4, 1, 1, 0, 1} // handshake declares >64 KiB
	tests := []struct {
		name               string
		wire               []byte
		private, forbidden bool
	}{
		{name: "malformed", wire: []byte("GET / HTTP/1.1\r\n\r\n")},
		{name: "oversize", wire: oversized},
		{name: "wireLimit", wire: fragmentHello(helloWithExtension(t, hello, 0xaaaa, make([]byte, 58000)))},
		{name: "ECH", wire: helloWithExtension(t, hello, 0xfe0d, []byte{0})},
		{name: "noSNI", wire: testHello(t, "127.0.0.1")},
		{name: "invalidSNI", wire: testHello(t, "bad_name.steamcontent.com")},
		{name: "forbiddenSNI", wire: testHello(t, "steamcontent.com.evil.example")},
		{name: "privateDNS", wire: hello, private: true},
		{name: "forbiddenPeer", wire: hello, forbidden: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			var dials atomic.Int64
			c.HomeDialContext = func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("unexpected dial")
			}
			c.PhoneDialContext = c.HomeDialContext
			if tt.private {
				c.LookupIP = func(context.Context, string) ([]net.IP, error) {
					return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("192.168.1.1")}, nil
				}
			}
			r := newRelay(t, c)
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var listener net.Listener = l
			if tt.forbidden {
				listener = peerOverrideListener{Listener: l}
			}
			done := make(chan error, 1)
			go func() { done <- r.ServeTLS(listener) }()
			defer func() { l.Close(); <-done }()
			conn, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			conn.Write(tt.wire)
			n, err := conn.Read(make([]byte, 1024))
			if n != 0 || err == nil {
				t.Fatalf("rejected hello received TLS alert/data: n=%d err=%v", n, err)
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("rejected hello was not promptly closed")
			}
			if dials.Load() != 0 {
				t.Fatal("invalid ingress reached a dial")
			}
			if s := r.Snapshot(); s.Home.Requests+s.Phone.Requests != 0 {
				t.Fatal("invalid ingress scheduled")
			}
		})
	}
}

type peerOverrideListener struct{ net.Listener }

func (l peerOverrideListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	return peerOverrideConn{c}, nil
}

type peerOverrideConn struct{ net.Conn }

func (c peerOverrideConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("192.168.22.1"), Port: 42}
}

func TestTLSFallbackOnlyBeforeAnyHelloByte(t *testing.T) {
	for _, mode := range []string{"dial", "zeroWrite", "partialWrite", "postHello"} {
		t.Run(mode, func(t *testing.T) {
			c := testConfig()
			c.HomeWeight, c.PhoneWeight = 1000, 1
			var written atomic.Int64
			r, client, _, _, phone := tlsIntegration(t, c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "verified") }), func(c *Config, addr string) {
				dial := c.HomeDialContext
				c.HomeDialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
					if mode == "dial" {
						return nil, errors.New("home unavailable")
					}
					if mode == "postHello" {
						a, b := net.Pipe()
						go func() { defer b.Close(); buf := make([]byte, 32<<10); n, _ := b.Read(buf); written.Add(int64(n)) }()
						return a, nil
					}
					conn, err := dial(ctx, network, target)
					if err != nil {
						return nil, err
					}
					return &failHelloConn{Conn: conn, partial: mode == "partialWrite", written: &written}, nil
				}
			})
			resp, err := client.Get("https://" + tlsTestHost + "/")
			if mode == "dial" || mode == "zeroWrite" {
				if err != nil {
					t.Fatal(err)
				}
				body, e := io.ReadAll(resp.Body)
				resp.Body.Close()
				if e != nil || string(body) != "verified" || phone.Load() != 1 {
					t.Fatalf("safe fallback failed: %q %v phone=%d", body, e, phone.Load())
				}
				waitBudget(t, func() bool { return r.Snapshot().Phone.Active == 0 })
			} else {
				if err == nil {
					resp.Body.Close()
					t.Fatal("partial stream unexpectedly succeeded")
				}
				waitBudget(t, func() bool { return r.Snapshot().Home.Active == 0 })
				if phone.Load() != 0 || written.Load() == 0 {
					t.Fatal("unsafe replay or no initial bytes")
				}
			}
		})
	}
}

type failHelloConn struct {
	net.Conn
	partial bool
	written *atomic.Int64
}

func (c *failHelloConn) Write(p []byte) (int, error) {
	if c.partial {
		n, err := c.Conn.Write(p[:1])
		c.written.Add(int64(n))
		if err != nil {
			return n, err
		}
		return n, errors.New("partial hello failure")
	}
	return 0, errors.New("before hello failure")
}

func TestTLSListenerAndRelayCloseCancelSessions(t *testing.T) {
	for _, closeRelay := range []bool{false, true} {
		t.Run(fmt.Sprint(closeRelay), func(t *testing.T) {
			c := testConfig()
			c.RequestTimeout = time.Hour
			c.HeaderTimeout = time.Hour
			r := newRelay(t, c)
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- r.ServeTLS(l) }()
			conn, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.Write([]byte{22, 3}) // parser blocked on incomplete record
			waitBudget(t, func() bool { return len(r.slots) == 1 })
			if closeRelay {
				r.Close()
			} else {
				l.Close()
			}
			select {
			case err := <-done:
				if !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
			case <-time.After(time.Second):
				t.Fatal("listener exit did not cancel session")
			}
			if len(r.slots) != 0 {
				t.Fatal("session slot leaked")
			}
			conn.SetReadDeadline(time.Now().Add(time.Second))
			n, err := conn.Read(make([]byte, 1))
			if n != 0 || err == nil {
				t.Fatal("client not closed")
			}
		})
	}
}

func TestTLSStreamHalfClosePreservesCapturedBytes(t *testing.T) {
	hello := fragmentHello(testHello(t, tlsTestHost))
	payload := bytes.Repeat([]byte{0xff, 0, 1}, 30000)
	response := []byte("response after upload EOF")
	origin, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	received := make(chan []byte, 1)
	go func() {
		conn, e := origin.Accept()
		if e != nil {
			received <- nil
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		got, _ := io.ReadAll(conn)
		received <- got
		conn.Write(response)
		closeWrite(conn)
	}()
	c := testConfig()
	c.HomeWeight, c.PhoneWeight = 1000, 1
	c.HomeDialContext = func(ctx context.Context, _, target string) (net.Conn, error) {
		if target != "8.8.8.8:443" {
			t.Error(target)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Addr().String())
	}
	r := newRelay(t, c)
	l, _ := startTLSIngress(t, r)
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	all := append(append([]byte(nil), hello...), payload...)
	if _, err := writeHello(conn, all); err != nil {
		t.Fatal(err)
	}
	closeWrite(conn)
	got, err := io.ReadAll(conn)
	if err != nil || !bytes.Equal(got, response) {
		t.Fatalf("half-close lost response: %q %v", got, err)
	}
	if got := <-received; !bytes.Equal(got, all) {
		t.Fatalf("captured/pipelined bytes changed: got=%d want=%d", len(got), len(all))
	}
}

func TestPhoneEOFKeepsWriteDirectionAndReservation(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	received := make(chan string, 1)
	go func() {
		conn, e := l.Accept()
		if e != nil {
			received <- ""
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, e = http.ReadRequest(bufio.NewReader(conn))
		if e != nil {
			received <- ""
			return
		}
		io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
		closeWrite(conn)
		data, _ := io.ReadAll(conn)
		received <- string(data)
	}()
	c := testConfig()
	c.PhoneProxyAddr = l.Addr().String()
	r := newRelay(t, c)
	conn, err := r.connectPhone(context.Background(), (&net.Dialer{}).DialContext, "8.8.8.8:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
	if phoneUsed(r) != 1 {
		t.Fatal("EOF prematurely released socket budget")
	}
	if _, err := io.WriteString(conn, "after EOF"); err != nil {
		t.Fatal(err)
	}
	if err := closeWrite(conn); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != "after EOF" {
		t.Fatalf("EOF destroyed write half: %q", got)
	}
	conn.Close()
	if phoneUsed(r) != 0 {
		t.Fatal("explicit close leaked budget")
	}
}

func TestTLSHeaderTimeoutAndSharedHTTPSlots(t *testing.T) {
	c := testConfig()
	c.MaxConcurrent = 1
	c.HeaderTimeout = 80 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
			return goodResponse("ok"), nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	})
	c.HomeTransport, c.PhoneTransport = transport, transport
	r := newRelay(t, c)
	l, _ := startTLSIngress(t, r)
	httpDone := make(chan struct{})
	go func() { defer close(httpDone); r.ServeHTTP(httptest.NewRecorder(), localRequest("GET", "/hold")) }()
	<-entered
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(time.Second))
	conn.Write(testHello(t, tlsTestHost))
	n, err := conn.Read(make([]byte, 1))
	conn.Close()
	if n != 0 || err == nil {
		t.Fatal("TLS bypassed HTTP concurrency slot")
	}
	once.Do(func() { close(release) })
	<-httpDone
	conn, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte{22, 3})
	waitBudget(t, func() bool { return len(r.slots) == 1 })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/busy"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("HTTP bypassed TLS concurrency slot")
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	n, err = conn.Read(make([]byte, 1))
	if n != 0 || err == nil {
		t.Fatal("incomplete hello survived HeaderTimeout")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("client timeout, not relay header timeout")
	}
	waitBudget(t, func() bool { return len(r.slots) == 0 })
}

func TestTLSIdleLifetimeAndActiveListenerCancellation(t *testing.T) {
	for _, mode := range []string{"idle", "lifetime", "listener", "relay"} {
		t.Run(mode, func(t *testing.T) {
			c := testConfig()
			if mode == "idle" {
				c.IdleTimeout = 150 * time.Millisecond
			}
			if mode == "lifetime" {
				c.RequestTimeout = 300 * time.Millisecond
			}
			entered := make(chan struct{}, 1)
			r, client, l, _, _ := tlsIntegration(t, c, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { entered <- struct{}{}; <-req.Context().Done() }), nil)
			done := make(chan error, 1)
			go func() {
				resp, err := client.Get("https://" + tlsTestHost + "/hold")
				if err == nil {
					resp.Body.Close()
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("verified TLS request did not reach origin")
			}
			if mode == "listener" {
				l.Close()
			}
			if mode == "relay" {
				r.Close()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unfinished response succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("session bound/cancellation failed")
			}
			waitBudget(t, func() bool {
				s := r.Snapshot()
				return s.Home.Active+s.Phone.Active == 0 && phoneUsed(r) == 0 && len(r.slots) == 0
			})
			requireHealthyPhone(t, r)
		})
	}
}

func TestTLSActiveDownloadDoesNotIdleOutQuietUpload(t *testing.T) {
	c := testConfig()
	c.IdleTimeout = 150 * time.Millisecond
	payload := bytes.Repeat([]byte("stream"), 100)
	_, client, _, _, _ := tlsIntegration(t, c, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		for i := 0; i < 10; i++ {
			if _, err := w.Write(payload); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-time.After(40 * time.Millisecond):
			case <-req.Context().Done():
				return
			}
		}
	}), nil)
	resp, err := client.Get("https://" + tlsTestHost + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, bytes.Repeat(payload, 10)) {
		t.Fatalf("quiet upload terminated progressing download: bytes=%d err=%v", len(got), err)
	}
}

func TestTLSPhoneCONNECTFailureFallsBackHome(t *testing.T) {
	r, client, _, home, phone := tlsIntegration(t, testConfig(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "home") }), func(c *Config, _ string) {
		c.PhoneDialContext = func(context.Context, string, string) (net.Conn, error) {
			a, b := net.Pipe()
			go func() {
				defer b.Close()
				http.ReadRequest(bufio.NewReader(b))
				io.WriteString(b, "HTTP/1.1 407 Denied\r\n\r\n")
			}()
			return a, nil
		}
	})
	resp, err := client.Get("https://" + tlsTestHost + "/")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(got) != "home" || home.Load() != 1 || phone.Load() != 0 {
		t.Fatalf("CONNECT early fallback failed: %q %v", got, err)
	}
	waitBudget(t, func() bool { return r.Snapshot().Home.Active == 0 })
	if s := r.Snapshot(); s.Phone.Errors != 1 || s.Phone.CooldownUntil.IsZero() || phoneUsed(r) != 0 {
		t.Fatalf("bad CONNECT failure accounting: %+v", s)
	}
}

func TestTLSProgressCountersVisibleBeforeStreamCloses(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	r, client, _, _, _ := tlsIntegration(t, testConfig(), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("stream-progress"), 2048))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}), nil)
	resp, err := client.Get("https://" + tlsTestHost + "/chunk/progress")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err = io.ReadFull(resp.Body, make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	waitBudget(t, func() bool {
		snapshot := r.Snapshot()
		return snapshot.Home.Bytes+snapshot.Phone.Bytes > 0 && snapshot.Home.Active+snapshot.Phone.Active > 0
	})
	once.Do(func() { close(release) })
	if _, err = io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
}
