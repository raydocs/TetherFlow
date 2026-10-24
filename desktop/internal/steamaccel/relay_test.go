package steamaccel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testConfig() Config {
	return Config{HomeIPv4: "127.0.0.1", PhoneToken: "test-secret", LookupIP: func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}}
}
func newRelay(t *testing.T, c Config) *Relay {
	t.Helper()
	r, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func localRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, "http://cache.steamcontent.com"+target, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	return req
}
func goodResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

// Real TCP origin and HTTP CONNECT proxy; test dial seam maps vetted addresses to
// the local origin only. Production origin validation is NOT disabled for tests.
func integrationRelay(t *testing.T, handler http.Handler) (*Relay, *http.Client, string, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	origin := httptest.NewServer(handler)
	t.Cleanup(origin.Close)
	originAddr := strings.TrimPrefix(origin.URL, "http://")
	var home, phone atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		phone.Add(1)
		if req.Method != "CONNECT" || req.Host != "8.8.8.8:80" || req.Header.Get("Proxy-Authorization") != "Bearer test-secret" {
			t.Errorf("invalid CONNECT: %s %s auth=%q", req.Method, req.Host, req.Header.Get("Proxy-Authorization"))
			http.Error(w, "bad CONNECT", 407)
			return
		}
		upstream, err := net.Dial("tcp", originAddr)
		if err != nil {
			t.Error(err)
			http.Error(w, "origin", 502)
			return
		}
		defer upstream.Close()
		downstream, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer downstream.Close()
		if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { io.Copy(upstream, rw); upstream.Close(); close(done) }()
		io.Copy(downstream, upstream)
		downstream.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	c := testConfig()
	c.PhoneProxyAddr = strings.TrimPrefix(proxy.URL, "http://")
	c.HomeDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		home.Add(1)
		if network != "tcp4" || address != "8.8.8.8:80" {
			t.Errorf("home not DNS pinned: %s %s", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", originAddr)
	}
	r := newRelay(t, c)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	// Close relay's pooled origin sockets before closing the hijacking proxy.
	t.Cleanup(func() { r.Close() })
	return r, client, server.URL, &home, &phone
}

func doContent(t *testing.T, client *http.Client, base, method string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, base+"/depot/123/chunk/abc?x=%2F", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "cache.steamcontent.com"
	req.Header = headers
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestRealRelayRangeHeadAndHeaders(t *testing.T) {
	payload := bytes.Repeat([]byte{0, 0xff, 1, 0x80, 'a', 'b'}, 20000)
	r, client, base, _, _ := integrationRelay(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Host != "cache.steamcontent.com" || req.URL.RequestURI() != "/depot/123/chunk/abc?x=%2F" {
			t.Errorf("target changed: %s %s", req.Host, req.URL)
		}
		if req.Header.Get("Range") != "bytes=4-120003" || req.Header.Get("If-Range") != `"chunk-etag"` || req.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("range headers changed: %v", req.Header)
		}
		for _, name := range []string{"X-Remove", "Proxy-Authorization", "Proxy-Connection", "Te", "Upgrade"} {
			if req.Header.Get(name) != "" {
				t.Errorf("leaked %s", name)
			}
		}
		w.Header().Set("Connection", "X-Origin-Secret")
		w.Header().Set("X-Origin-Secret", "private")
		w.Header().Set("Proxy-Authenticate", "private")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Header().Set("Content-Range", "bytes 4-120003/999999")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"chunk-etag"`)
		w.WriteHeader(206)
		if req.Method != "HEAD" {
			w.Write(payload)
		}
	}))
	headers := http.Header{"Range": {"bytes=4-120003"}, "If-Range": {`"chunk-etag"`}, "Accept-Encoding": {"identity"},
		"Connection": {"X-Remove, Upgrade"}, "X-Remove": {"private"}, "Proxy-Authorization": {"Bearer client-secret"}, "Proxy-Connection": {"keep-alive"}, "Te": {"trailers"}, "Upgrade": {"websocket"}}
	for _, method := range []string{"GET", "HEAD", "GET", "GET", "GET", "GET"} {
		resp, got := doContent(t, client, base, method, headers)
		if resp.StatusCode != 206 || resp.Header.Get("ETag") != `"chunk-etag"` || resp.Header.Get("Content-Range") != "bytes 4-120003/999999" || resp.Header.Get("Accept-Ranges") != "bytes" {
			t.Fatalf("response headers changed: %v", resp)
		}
		for _, h := range []string{"Connection", "X-Origin-Secret", "Proxy-Authenticate"} {
			if resp.Header.Get(h) != "" {
				t.Fatalf("hop header leaked: %s", h)
			}
		}
		if method == "HEAD" {
			if len(got) != 0 || resp.ContentLength != int64(len(payload)) {
				t.Fatal("HEAD changed")
			}
		} else if !bytes.Equal(got, payload) {
			t.Fatal("body not byte identical")
		}
	}
	s := r.Snapshot()
	if s.Home.Requests == 0 || s.Phone.Requests == 0 || s.Home.Bytes+s.Phone.Bytes != uint64(5*len(payload)) || s.Home.Active+s.Phone.Active != 0 {
		t.Fatalf("bad counters: %+v", s)
	}
}

func TestBothPathsUnderConcurrentRealDownloads(t *testing.T) {
	const count = 20
	entered := make(chan struct{}, count)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	payload := bytes.Repeat([]byte("streamed chunk\x00"), 4000)
	r, client, base, home, phone := integrationRelay(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		w.Write(payload)
	}))
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", base+"/chunk", nil)
			req.Host = "cache.steamcontent.com"
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, payload) {
				errs <- errors.New("different response bytes")
			}
		}()
	}
	for i := 0; i < count; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent downloads did not reach origins")
		}
	}
	s := r.Snapshot()
	if s.Home.Active != 4 || s.Phone.Active != 16 {
		t.Errorf("weighted least-outstanding 1:4: %+v", s)
	}
	if home.Load() == 0 || phone.Load() == 0 {
		t.Error("one real uplink never used")
	}
	once.Do(func() { close(release) })
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	s = r.Snapshot()
	if s.Home.Errors+s.Phone.Errors != 0 || s.Home.Bytes+s.Phone.Bytes != uint64(count*len(payload)) {
		t.Fatalf("bad status: %+v", s)
	}
}

func TestWeightedSequentialAndCustomWeights(t *testing.T) {
	for _, weights := range [][2]int{{1, 4}, {3, 2}} {
		c := testConfig()
		c.HomeWeight, c.PhoneWeight = weights[0], weights[1]
		c.HomeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { return goodResponse("ok"), nil })
		c.PhoneTransport = c.HomeTransport
		r := newRelay(t, c)
		for i := 0; i < 100; i++ {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, localRequest("GET", "/"))
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
		}
		s := r.Snapshot()
		wantHome := uint64(100 * weights[0] / (weights[0] + weights[1]))
		if s.Home.Requests != wantHome || s.Phone.Requests != 100-wantHome {
			t.Fatalf("weights %v: %+v", weights, s)
		}
	}
}

func TestFailoverAndCooldown(t *testing.T) {
	for _, mode := range []string{"dial", "503", "oversize", "nil", "badstatus"} {
		t.Run(mode, func(t *testing.T) {
			c := testConfig()
			c.HomeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { return goodResponse("home body"), nil })
			c.PhoneTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				switch mode {
				case "dial":
					return nil, errors.New("phone offline")
				case "nil":
					return nil, nil
				case "503":
					resp := goodResponse("unavailable")
					resp.StatusCode = 503
					return resp, nil
				case "badstatus":
					resp := goodResponse("bad")
					resp.StatusCode = 101
					return resp, nil
				default:
					resp := goodResponse("large")
					resp.Header.Set("X-Large", strings.Repeat("x", 40000))
					return resp, nil
				}
			})
			r := newRelay(t, c)
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, localRequest("GET", "/"))
				if w.Code != 200 || w.Body.String() != "home body" {
					t.Fatalf("failover: %d %s", w.Code, w.Body)
				}
			}
			s := r.Snapshot()
			if s.Phone.Requests != 1 || s.Phone.Errors != 1 || !s.Phone.CooldownUntil.After(time.Now()) || s.Home.Requests != 2 || s.Home.Active+s.Phone.Active != 0 {
				t.Fatalf("cooldown: %+v", s)
			}
		})
	}
}

func TestCancellationDoesNotPoisonHealth(t *testing.T) {
	c := testConfig()
	entered := make(chan struct{})
	c.PhoneTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	c.HomeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("canceled request retried")
		return goodResponse("bad"), nil
	})
	r := newRelay(t, c)
	req := localRequest("GET", "/")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); r.ServeHTTP(httptest.NewRecorder(), req) }()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock")
	}
	s := r.Snapshot()
	if s.Home.Requests != 0 || s.Phone.Errors != 0 || !s.Phone.CooldownUntil.IsZero() || s.Phone.Active != 0 {
		t.Fatalf("cancellation poisoned: %+v", s)
	}
}

type partialBody struct{ sent bool }

func (b *partialBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "prefix"), nil
	}
	return 0, io.ErrUnexpectedEOF
}
func (*partialBody) Close() error { return nil }
func TestNoRetryAfterResponseCommit(t *testing.T) {
	c := testConfig()
	c.PhoneTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		resp := goodResponse("")
		resp.Body = &partialBody{}
		resp.ContentLength = -1
		return resp, nil
	})
	c.HomeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("partial response retried")
		return goodResponse("suffix"), nil
	})
	r := newRelay(t, c)
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if p := recover(); p != http.ErrAbortHandler {
				t.Errorf("expected abort, got %v", p)
			}
		}()
		r.ServeHTTP(w, localRequest("GET", "/"))
	}()
	if w.Body.String() != "prefix" {
		t.Fatal(w.Body.String())
	}
	s := r.Snapshot()
	if s.Phone.Errors != 1 || s.Home.Requests != 0 || s.Phone.Bytes != 6 {
		t.Fatal(s)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	c := testConfig()
	c.MaxConcurrent = 1
	entered := make(chan struct{})
	release := make(chan struct{})
	c.PhoneTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { close(entered); <-release; return goodResponse("ok"), nil })
	c.HomeTransport = c.PhoneTransport
	r := newRelay(t, c)
	done := make(chan struct{})
	go func() { r.ServeHTTP(httptest.NewRecorder(), localRequest("GET", "/")); close(done) }()
	<-entered
	w := httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/"))
	close(release)
	<-done
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestSecurityHostsPeersDNSAndPorts(t *testing.T) {
	forbiddenHosts := []string{"evil.com", "steamcontent.com.evil.com", "evilsteamcontent.com", "steamcontent.com.", "127.0.0.1", "2130706433", "cache.steamcontent.com:81", "cache.steamcontent.com:0443", "cache.steamcontent.com:0", "cache.steamcontent.com@evil.com", "cache.steamcontent.com%00", "cache..steamcontent.com", "cache.steamcontent.com/evil", "cache.steamcontent.com:443:80", "[::1]:80", "cache.steamcontent.com:65536"}
	for _, host := range forbiddenHosts {
		t.Run(host, func(t *testing.T) {
			c := testConfig()
			c.HomeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("forbidden origin reached"); return nil, nil })
			c.PhoneTransport = c.HomeTransport
			r := newRelay(t, c)
			req := localRequest("GET", "/")
			req.Host = host
			req.URL.Host = ""
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != 403 {
				t.Fatalf("host %q: %d", host, w.Code)
			}
		})
	}
	for _, peer := range []string{"192.168.1.2:123", "8.8.8.8:12", "invalid", ":123", "[fe80::1%lo0]:123"} {
		c := testConfig()
		r := newRelay(t, c)
		req := localRequest("GET", "/")
		req.RemoteAddr = peer
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("peer %s: %d", peer, w.Code)
		}
	}
	private := []string{"127.0.0.1", "10.0.0.1", "172.16.1.1", "192.168.1.1", "169.254.169.254", "100.64.1.1", "0.0.0.0", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::7f00:1", "2001:db8::1", "2002:7f00:1::1", "2001::1", "3fff::1"}
	for _, ip := range private {
		t.Run(ip, func(t *testing.T) {
			c := testConfig()
			c.LookupIP = func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP(ip)}, nil
			}
			c.HomeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("private/mixed DNS reached origin")
				return nil, errors.New("bad")
			})
			c.PhoneTransport = c.HomeTransport
			r := newRelay(t, c)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, localRequest("GET", "/"))
			if w.Code != 403 {
				t.Fatal(w.Code)
			}
		})
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !publicIP(net.ParseIP(ip)) {
			t.Errorf("public rejected: %s", ip)
		}
	}
}

func TestExplicitHomePeerAndCustomAllowlist(t *testing.T) {
	for _, allow := range []bool{false, true} {
		c := testConfig()
		c.HomeIPv4 = "192.168.1.10"
		c.AllowHomePeer = allow
		c.AllowedHosts = []string{"custom.example"}
		c.HomeDialContext = (&net.Dialer{}).DialContext
		c.HomeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { return goodResponse("ok"), nil })
		c.PhoneTransport = c.HomeTransport
		r := newRelay(t, c)
		for _, peer := range []string{"192.168.1.10:123", "192.168.1.11:123", "127.0.0.1:123", "[::1]:123"} {
			req := localRequest("GET", "/")
			req.Host = "CUSTOM.EXAMPLE:80"
			req.URL.Host = ""
			req.RemoteAddr = peer
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			want := 403
			if strings.HasPrefix(peer, "127.") || strings.HasPrefix(peer, "[::1]") || allow && peer == "192.168.1.10:123" {
				want = 200
			}
			if w.Code != want {
				t.Errorf("allow %v peer %s: %d", allow, peer, w.Code)
			}
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, localRequest("GET", "/"))
		if w.Code != 403 {
			t.Fatal("custom allowlist did not replace defaults")
		}
	}
}

func TestRequestValidationAndServerBounds(t *testing.T) {
	c := testConfig()
	r := newRelay(t, c)
	for _, method := range []string{"POST", "CONNECT", "PUT", "OPTIONS"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, localRequest(method, "/"))
		if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal(method, w.Code)
		}
	}
	cases := []func(*http.Request){func(req *http.Request) { req.ContentLength = 1 }, func(req *http.Request) { req.TransferEncoding = []string{"chunked"} }, func(req *http.Request) { req.URL.Scheme = "https" }, func(req *http.Request) { req.URL.Host = "evil.com" }, func(req *http.Request) { req.URL.Opaque = "//evil.com" }}
	for _, mutate := range cases {
		req := localRequest("GET", "/")
		mutate(req)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	req := localRequest("GET", "/")
	req.Header.Set("X-Large", strings.Repeat("x", 40000))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 431 {
		t.Fatal(w.Code)
	}
	s := r.Server("127.0.0.1:80")
	if s.Handler != r || s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 || s.MaxHeaderBytes != 32<<10 {
		t.Fatal("unbounded server")
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []func(*Config){func(c *Config) { c.HomeIPv4 = "" }, func(c *Config) { c.HomeIPv4 = "::1" }, func(c *Config) { c.PhoneToken = "bad\r\nAuth:bad" }, func(c *Config) { c.PhoneToken = "" }, func(c *Config) { c.PhoneProxyAddr = "localhost:8283" }, func(c *Config) { c.PhoneProxyAddr = "192.168.1.1:8283" }, func(c *Config) { c.PhoneProxyAddr = "127.0.0.1:0" }, func(c *Config) { c.HomeWeight = 1; c.PhoneWeight = 0 }, func(c *Config) { c.AllowedHosts = []string{} }, func(c *Config) { c.AllowedHosts = []string{"*.steamcontent.com"} }, func(c *Config) { c.MaxConcurrent = -1 }, func(c *Config) { c.MaxHeaderBytes = 1 }, func(c *Config) { c.RequestTimeout = -time.Second }}
	for i, mutate := range cases {
		c := testConfig()
		mutate(&c)
		r, err := New(c)
		if err == nil {
			r.Close()
			t.Errorf("config %d accepted", i)
		}
	}
}

func TestDNSRebindingCheckedOnEveryRequest(t *testing.T) {
	c := testConfig()
	var calls atomic.Int64
	c.LookupIP = func(context.Context, string) ([]net.IP, error) {
		if calls.Add(1) == 1 {
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	c.HomeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { return goodResponse("ok"), nil })
	c.PhoneTransport = c.HomeTransport
	r := newRelay(t, c)
	for i, want := range []int{200, 403} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, localRequest("GET", "/"))
		if w.Code != want {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if s := r.Snapshot(); s.Home.Requests+s.Phone.Requests != 1 {
		t.Fatal(s)
	}
}

func TestCloseCancelsAndRejectsNewRequests(t *testing.T) {
	c := testConfig()
	entered := make(chan struct{})
	c.PhoneTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	r := newRelay(t, c)
	done := make(chan struct{})
	go func() { r.ServeHTTP(httptest.NewRecorder(), localRequest("GET", "/")); close(done) }()
	<-entered
	r.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close failed to cancel")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/"))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	if s := r.Snapshot(); s.Phone.Errors != 0 || s.Phone.Active != 0 {
		t.Fatal(s)
	}
}

func TestOriginDialRejectsUnpinnedAndPins443(t *testing.T) {
	c := testConfig()
	r := newRelay(t, c)
	var got string
	dial := r.originDial(func(_ context.Context, _, addr string) (net.Conn, error) { got = addr; return nil, errors.New("done") }, false)
	if _, err := dial(context.Background(), "tcp", "example.com:80"); err == nil {
		t.Fatal("unpinned dial accepted")
	}
	ctx := context.WithValue(context.Background(), targetKey{}, dialTarget{host: "cache.steamcontent.com", port: "443", ips: []net.IP{net.ParseIP("8.8.8.8")}})
	dial(ctx, "tcp", "cache.steamcontent.com:443")
	if got != "8.8.8.8:443" {
		t.Fatal(got)
	}
}

func TestDefaultHomeDialerSourceBinding(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c := testConfig()
	r := newRelay(t, c)
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	// Internal dial test only: public-address checks are separately enforced by ServeHTTP.
	ctx := context.WithValue(context.Background(), targetKey{}, dialTarget{host: "cache.steamcontent.com", port: port, ips: []net.IP{net.ParseIP("127.0.0.1")}})
	transport := r.paths[0].transport.(*http.Transport)
	conn, err := transport.DialContext(ctx, "tcp", net.JoinHostPort("cache.steamcontent.com", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if ip, _, _ := net.SplitHostPort(peer.RemoteAddr().String()); ip != "127.0.0.1" {
		t.Fatal("wrong source", ip)
	}
}

func ExampleRelay_Snapshot() {
	// An embedding command owns discovery/DNS and listener lifecycle.
	r, err := New(Config{HomeIPv4: "127.0.0.1", PhoneToken: "example-token"})
	if err != nil {
		panic(err)
	}
	defer r.Close()
	fmt.Println(r.Snapshot().Phone.Requests)
	// Output: 0
}

// Compile-time checks for standard integration surfaces.
var _ http.Handler = (*Relay)(nil)
var _ io.Closer = (*Relay)(nil)

type contextBody struct {
	ctx     context.Context
	first   bool
	entered chan struct{}
}

func (b *contextBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		return copy(p, "prefix"), nil
	}
	close(b.entered)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (*contextBody) Close() error { return nil }

func TestCancellationDuringBodyDoesNotPoisonHealth(t *testing.T) {
	c := testConfig()
	entered := make(chan struct{})
	c.PhoneTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := goodResponse("")
		resp.Body = &contextBody{ctx: req.Context(), entered: entered}
		return resp, nil
	})
	c.HomeTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("body cancellation retried")
		return goodResponse("bad"), nil
	})
	r := newRelay(t, c)
	req := localRequest("GET", "/")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != http.ErrAbortHandler {
				t.Errorf("unexpected abort: %v", p)
			}
		}()
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("body cancellation stalled")
	}
	s := r.Snapshot()
	if s.Home.Requests != 0 || s.Phone.Errors != 0 || s.Phone.Bytes != 6 || s.Phone.Active != 0 || !s.Phone.CooldownUntil.IsZero() {
		t.Fatal(s)
	}
}

func TestRequestTimeoutReturns504BeforeCommit(t *testing.T) {
	c := testConfig()
	c.RequestTimeout = 20 * time.Millisecond
	c.PhoneTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	r := newRelay(t, c)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/"))
	if w.Code != 504 {
		t.Fatalf("timeout returned %d", w.Code)
	}
	if s := r.Snapshot(); s.Phone.Errors != 1 || s.Home.Requests != 0 {
		t.Fatal(s)
	}
}

func TestBothPathsCoolingDownThenRecover(t *testing.T) {
	c := testConfig()
	c.FailureCooldown = 100 * time.Millisecond
	var fail atomic.Bool
	fail.Store(true)
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		if fail.Load() {
			return nil, errors.New("unavailable")
		}
		return goodResponse("recovered"), nil
	})
	c.HomeTransport = transport
	c.PhoneTransport = transport
	r := newRelay(t, c)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/"))
	if w.Code != 502 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/"))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	fail.Store(false)
	time.Sleep(120 * time.Millisecond)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/"))
	if w.Code != 200 || w.Body.String() != "recovered" {
		t.Fatal(w.Code, w.Body)
	}
}

func TestCleanHeadersCaseInsensitiveAndNoMutation(t *testing.T) {
	original := http.Header{"connection": {"x-remove, eTAG"}, "x-remove": {"secret"}, "ETag": {"remove"}, "proxy-authorization": {"secret"}, "range": {"bytes=0-1"}, "Content-Range": {"bytes 0-1/3"}}
	cleaned := cleanHeaders(original)
	if len(cleaned) != 2 || cleaned.Get("Range") != "bytes=0-1" || cleaned.Get("Content-Range") != "bytes 0-1/3" {
		t.Fatal(cleaned)
	}
	cleaned["Range"][0] = "changed"
	if original["range"][0] != "bytes=0-1" || len(original) != 6 {
		t.Fatal("mutated original")
	}
}

func TestOrigin443UsesTLSAndPreservesHost(t *testing.T) {
	c := testConfig()
	c.HomeTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != "cache.steamcontent.com:443" || req.Host != "cache.steamcontent.com:443" {
			t.Error("incorrect TLS target", req.URL, req.Host)
		}
		return goodResponse("ok"), nil
	})
	c.PhoneTransport = c.HomeTransport
	r := newRelay(t, c)
	req := localRequest("GET", "/")
	req.Host = "cache.steamcontent.com:443"
	req.URL.Host = ""
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestStreamsBeforeOriginCompletes(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	_, client, base, _, _ := integrationRelay(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write(bytes.Repeat([]byte("a"), 32<<10))
		w.(http.Flusher).Flush()
		close(entered)
		select {
		case <-release:
			w.Write([]byte("tail"))
		case <-req.Context().Done():
		}
	}))
	req, _ := http.NewRequest("GET", base+"/chunk", nil)
	req.Host = "cache.steamcontent.com"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
	prefix := make([]byte, 32<<10)
	if _, err := io.ReadFull(resp.Body, prefix); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prefix, bytes.Repeat([]byte("a"), 32<<10)) {
		t.Fatal("prefix corrupted")
	}
	once.Do(func() { close(release) })
	tail, err := io.ReadAll(resp.Body)
	if err != nil || string(tail) != "tail" {
		t.Fatal(string(tail), err)
	}
}

func TestDetachedDialsAreBoundedAndCloseCancelsThem(t *testing.T) {
	c := testConfig()
	c.MaxConcurrent = 1
	r := newRelay(t, c)
	entered := make(chan struct{})
	var calls atomic.Int64
	dial := r.originDial(func(ctx context.Context, _, _ string) (net.Conn, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, false)
	ctx := context.WithValue(context.Background(), targetKey{}, dialTarget{host: "cache.steamcontent.com", port: "80", ips: []net.IP{net.ParseIP("8.8.8.8")}})
	done := make(chan error, 1)
	go func() { _, err := dial(ctx, "tcp", "cache.steamcontent.com:80"); done <- err }()
	<-entered
	if _, err := dial(ctx, "tcp", "cache.steamcontent.com:80"); !errors.Is(err, errDialBusy) {
		t.Fatal("unbounded concurrent dial", err)
	}
	r.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached dial survived Close")
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestDialCapacityDoesNotPoisonPathHealth(t *testing.T) {
	c := testConfig()
	c.PhoneTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errDialBusy })
	r := newRelay(t, c)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/"))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	s := r.Snapshot()
	if s.Home.Requests != 0 || s.Phone.Errors != 0 || s.Phone.Active != 0 || !s.Phone.CooldownUntil.IsZero() {
		t.Fatal(s)
	}
}
