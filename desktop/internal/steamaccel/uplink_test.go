package steamaccel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// gatedBody delivers its chunks, then blocks until release is closed.
type gatedBody struct {
	chunks  []string
	release chan struct{}
	ctx     context.Context
}

func (b *gatedBody) Read(p []byte) (int, error) {
	if len(b.chunks) > 0 {
		n := copy(p, b.chunks[0])
		b.chunks = b.chunks[1:]
		return n, nil
	}
	select {
	case <-b.release:
		return 0, io.EOF
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}
func (*gatedBody) Close() error { return nil }

func TestHTTPBytesVisibleBeforeBodyCompletes(t *testing.T) {
	release := make(chan struct{})
	gated := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := goodResponse("")
		resp.ContentLength = -1
		resp.Body = &gatedBody{chunks: []string{strings.Repeat("a", 100), strings.Repeat("b", 28)}, release: release, ctx: req.Context()}
		return resp, nil
	})
	c := testConfig()
	c.HomeTransport, c.PhoneTransport = gated, gated
	r := newRelay(t, c)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer func() { recover(); close(done) }(); r.ServeHTTP(w, localRequest("GET", "/")) }()
	total := func() (uint64, uint64) {
		s := r.Snapshot()
		return s.Home.Bytes + s.Phone.Bytes, s.Home.Active + s.Phone.Active
	}
	waitBudget(t, func() bool { b, _ := total(); return b == 128 })
	if _, active := total(); active != 1 {
		t.Fatalf("body must still be in flight, active=%d", active)
	}
	close(release)
	<-done
	// Bytes are added once per write: finishing the stream must not add them again.
	if b, active := total(); b != 128 || active != 0 {
		t.Fatalf("bytes=%d active=%d, want 128 and 0", b, active)
	}
	if len(w.Body.String()) != 128 {
		t.Fatalf("body %d bytes", w.Body.Len())
	}
}

func failTransport(t *testing.T, name string) roundTripFunc {
	return func(*http.Request) (*http.Response, error) {
		t.Errorf("%s transport used", name)
		return nil, errors.New("must not be used")
	}
}

func TestUplinkHomeOnlyNeverDialsPhone(t *testing.T) {
	var phoneDials atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close") // force a fresh home dial next time
		io.WriteString(w, "home-body")
	}))
	t.Cleanup(origin.Close)
	var failHome atomic.Bool
	c := testConfig()
	c.Uplinks = UplinkHomeOnly
	c.PhoneToken = ""
	c.PhoneDialContext = func(context.Context, string, string) (net.Conn, error) {
		phoneDials.Add(1)
		return nil, errors.New("phone dial in home-only mode")
	}
	c.PhoneTransport = failTransport(t, "phone")
	c.HomeDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		if failHome.Load() {
			return nil, errors.New("home down")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(origin.URL, "http://"))
	}
	r := newRelay(t, c)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/"))
	if w.Code != 200 || w.Body.String() != "home-body" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	// Even a home failure and a cooldown must not fall over to the phone.
	failHome.Store(true)
	for i := 0; i < 2; i++ {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, localRequest("GET", "/"))
		if w.Code < 500 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	s := r.Snapshot()
	if phoneDials.Load() != 0 || s.Phone.Requests != 0 || s.Phone.Errors != 0 || phoneUsed(r) != 0 || s.Home.Errors == 0 || s.Mode != UplinkHomeOnly {
		t.Fatalf("phone touched: dials=%d %+v", phoneDials.Load(), s)
	}
}

func TestUplinkHomeOnlyValidWithoutToken(t *testing.T) {
	c := testConfig()
	c.Uplinks = UplinkHomeOnly
	c.PhoneToken = ""
	r := newRelay(t, c)
	if r.Snapshot().Mode != UplinkHomeOnly {
		t.Fatalf("mode %v", r.Snapshot().Mode)
	}
	// A token is only required where the phone is used.
	for _, mode := range []UplinkMode{UplinkDual, UplinkPhoneOnly} {
		c.Uplinks = mode
		if x, err := New(c); err == nil {
			x.Close()
			t.Errorf("mode %v accepted without a token", mode)
		}
	}
	// A token that is supplied is still validated, even though it is unused.
	c.Uplinks, c.PhoneToken = UplinkHomeOnly, "bad\r\nAuth:bad"
	if x, err := New(c); err == nil {
		x.Close()
		t.Error("malformed token accepted")
	}
}

func TestUplinkPhoneOnlyNeverDialsHomeAndFailsClosed(t *testing.T) {
	var homeDials atomic.Int64
	c := testConfig()
	c.Uplinks = UplinkPhoneOnly
	c.HomeTransport = failTransport(t, "home")
	c.HomeDialContext = func(context.Context, string, string) (net.Conn, error) {
		homeDials.Add(1)
		return nil, errors.New("home dial in phone-only mode")
	}
	var phoneFails atomic.Int64
	c.PhoneTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		phoneFails.Add(1)
		return nil, errors.New("phone down")
	})
	r := newRelay(t, c)
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, localRequest("GET", "/"))
		if w.Code != http.StatusBadGateway && w.Code != http.StatusServiceUnavailable {
			t.Fatalf("request %d fail-open: %d", i, w.Code)
		}
	}
	s := r.Snapshot()
	if homeDials.Load() != 0 || s.Home.Requests != 0 || s.Home.Errors != 0 || s.Phone.Errors == 0 || s.Mode != UplinkPhoneOnly {
		t.Fatalf("home touched: %+v", s)
	}
	// While the phone cools down there is nothing to fall back to.
	if phoneFails.Load() != 1 {
		t.Fatalf("phone attempted %d times; single-path mode must not retry the same path", phoneFails.Load())
	}
}

func TestTLSRespectsUplinkMode(t *testing.T) {
	serve := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "origin") })
	t.Run("homeonly", func(t *testing.T) {
		c := testConfig()
		c.Uplinks, c.PhoneToken = UplinkHomeOnly, ""
		var phoneDials atomic.Int64
		r, client, _, home, phone := tlsIntegration(t, c, serve, func(c *Config, _ string) {
			c.PhoneDialContext = func(context.Context, string, string) (net.Conn, error) {
				phoneDials.Add(1)
				return nil, errors.New("phone dial in home-only mode")
			}
		})
		resp, err := client.Get("https://" + tlsTestHost + "/")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != "origin" || home.Load() != 1 || phone.Load() != 0 || phoneDials.Load() != 0 || r.Snapshot().Phone.Requests != 0 {
			t.Fatalf("%q home=%d phone=%d dials=%d %+v", got, home.Load(), phone.Load(), phoneDials.Load(), r.Snapshot())
		}
	})
	t.Run("phoneonly", func(t *testing.T) {
		c := testConfig()
		c.Uplinks = UplinkPhoneOnly
		r, client, _, home, phone := tlsIntegration(t, c, serve, nil)
		resp, err := client.Get("https://" + tlsTestHost + "/")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != "origin" || phone.Load() != 1 || home.Load() != 0 || r.Snapshot().Home.Requests != 0 {
			t.Fatalf("%q home=%d phone=%d %+v", got, home.Load(), phone.Load(), r.Snapshot())
		}
	})
	t.Run("phoneonly-failure-never-falls-back-home", func(t *testing.T) {
		c := testConfig()
		c.Uplinks = UplinkPhoneOnly
		r, client, _, home, _ := tlsIntegration(t, c, serve, func(c *Config, _ string) {
			c.PhoneDialContext = func(context.Context, string, string) (net.Conn, error) {
				a, b := net.Pipe()
				go func() {
					defer b.Close()
					http.ReadRequest(bufio.NewReader(b))
					io.WriteString(b, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
				}()
				return a, nil
			}
		})
		if resp, err := client.Get("https://" + tlsTestHost + "/"); err == nil {
			resp.Body.Close()
			t.Fatal("request succeeded with the phone down")
		}
		waitBudget(t, func() bool { s := r.Snapshot(); return s.Phone.Active == 0 && s.Phone.Errors >= 1 })
		if s := r.Snapshot(); home.Load() != 0 || s.Home.Requests != 0 || s.Home.Errors != 0 {
			t.Fatalf("home used in phone-only mode: %+v", s)
		}
	})
}

func TestConfigRejectsUnknownUplinkMode(t *testing.T) {
	for _, mode := range []UplinkMode{-1, 3, 99} {
		c := testConfig()
		c.Uplinks = mode
		if r, err := New(c); err == nil {
			r.Close()
			t.Errorf("mode %d accepted", mode)
		}
	}
	for mode, want := range map[UplinkMode]string{UplinkDual: "dual", UplinkHomeOnly: "homeonly", UplinkPhoneOnly: "phoneonly"} {
		c := testConfig()
		c.Uplinks = mode
		r := newRelay(t, c)
		if r.Snapshot().Mode != mode || mode.String() != want {
			t.Errorf("mode %v snapshot %v", mode, r.Snapshot().Mode)
		}
	}
}
