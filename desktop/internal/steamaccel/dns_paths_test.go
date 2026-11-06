package steamaccel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// r9Phone answers CONNECT over in-memory pipes. status decides each reply from
// the CONNECT authority; a 200 tunnels to originAddr. connects lists the
// authorities it saw, in order.
type r9Phone struct {
	mu       sync.Mutex
	connects []string
	status   func(authority string) int
	origin   string
}

func (p *r9Phone) dial(context.Context, string, string) (net.Conn, error) {
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		br := bufio.NewReader(b)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.connects = append(p.connects, req.Host)
		p.mu.Unlock()
		code := p.status(req.Host)
		if code != 200 {
			fmt.Fprintf(b, "HTTP/1.1 %d %s\r\n\r\n", code, http.StatusText(code))
			return
		}
		up, err := net.Dial("tcp", p.origin)
		if err != nil {
			io.WriteString(b, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			return
		}
		defer up.Close()
		io.WriteString(b, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { io.Copy(up, br); closeWrite(up); close(done) }()
		io.Copy(b, up)
		<-done
	}()
	return a, nil
}

func (p *r9Phone) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.connects...)
}

func statusAlways(code int) func(string) int { return func(string) int { return code } }

func plainOrigin(t *testing.T, body string) string {
	t.Helper()
	o := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }))
	t.Cleanup(o.Close)
	return strings.TrimPrefix(o.URL, "http://")
}

// homeToOrigin accepts only pinned numeric tcp4 targets and records them.
func homeToOrigin(t *testing.T, origin string, dials *atomic.Int64) DialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if host, _, err := net.SplitHostPort(address); network != "tcp4" || err != nil || net.ParseIP(host) == nil {
			t.Errorf("home dial not numeric tcp4: %s %s", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin)
	}
}

func serve(r *Relay) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/"))
	return w
}

func TestHomeDNSFailureStillServesViaPhone(t *testing.T) {
	for _, tc := range []struct {
		name         string
		home, phone  int
		wantLookups  int64
		wantHomeErrs uint64
	}{
		// Home is scheduled first: its DNS failure is a home error, then the phone serves.
		{"homeFirst", 4, 1, 1, 1},
		// Phone is scheduled first and never waits on, or even calls, the PC resolver.
		{"phoneFirst", 1, 4, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lookups atomic.Int64
			c := testConfig()
			c.HomeWeight, c.PhoneWeight = tc.home, tc.phone
			c.LookupIP = func(context.Context, string) ([]net.IP, error) {
				lookups.Add(1)
				return nil, errors.New("SERVFAIL")
			}
			phone := &r9Phone{status: statusAlways(200), origin: plainOrigin(t, "via-phone")}
			c.PhoneDialContext = phone.dial
			r := newRelay(t, c)
			if w := serve(r); w.Code != 200 || w.Body.String() != "via-phone" {
				t.Fatalf("%d %q", w.Code, w.Body.String())
			}
			if lookups.Load() != tc.wantLookups {
				t.Fatalf("lookups = %d, want %d", lookups.Load(), tc.wantLookups)
			}
			if s := r.Snapshot(); s.Home.Errors != tc.wantHomeErrs || s.Phone.Errors != 0 || (tc.wantHomeErrs == 1) == s.Home.CooldownUntil.IsZero() {
				t.Fatalf("health: %+v", s)
			}
		})
	}
}

func TestPhoneFailureStillServesViaHome(t *testing.T) {
	var homeDials atomic.Int64
	c := testConfig() // phone is scheduled first
	phone := &r9Phone{status: statusAlways(502)}
	c.PhoneDialContext = phone.dial
	c.HomeDialContext = homeToOrigin(t, plainOrigin(t, "via-home"), &homeDials)
	r := newRelay(t, c)
	if w := serve(r); w.Code != 200 || w.Body.String() != "via-home" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if s := r.Snapshot(); s.Phone.Errors != 1 || s.Phone.CooldownUntil.IsZero() || s.Home.Errors != 0 || homeDials.Load() != 1 {
		t.Fatalf("%+v dials=%d", s, homeDials.Load())
	}
}

func TestPhoneCONNECTUsesHostnameAndNeverLocalDNS(t *testing.T) {
	c := testConfig()
	c.Uplinks = UplinkPhoneOnly
	c.LookupIP = func(context.Context, string) ([]net.IP, error) {
		t.Error("phone path used the desktop resolver")
		return nil, errors.New("must not be called")
	}
	phone := &r9Phone{status: statusAlways(200), origin: plainOrigin(t, "ok")}
	c.PhoneDialContext = phone.dial
	r := newRelay(t, c)
	if w := serve(r); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if got := phone.seen(); len(got) != 1 || got[0] != "cache.steamcontent.com:80" {
		t.Fatalf("CONNECT authority: %v", got)
	}
}

func TestAllowlistRunsBeforePhoneCONNECT(t *testing.T) {
	for _, mode := range []UplinkMode{UplinkDual, UplinkPhoneOnly} {
		for _, host := range []string{"evil.com", "steamcontent.com.evil.com", "127.0.0.1", "cache.steamcontent.com:81"} {
			c := testConfig()
			c.Uplinks = mode
			c.LookupIP = func(context.Context, string) ([]net.IP, error) {
				t.Error("lookup before allowlist")
				return nil, errors.New("no")
			}
			phone := &r9Phone{status: statusAlways(200), origin: "127.0.0.1:1"}
			c.PhoneDialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
				t.Errorf("phone dialled for %q", host)
				return phone.dial(ctx, n, a)
			}
			r := newRelay(t, c)
			req := localRequest("GET", "/")
			req.Host, req.URL.Host = host, ""
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != 403 || len(phone.seen()) != 0 {
				t.Fatalf("%s %q: %d %v", mode, host, w.Code, phone.seen())
			}
		}
	}
}

func TestPhone403NoCooldownFallsBackHome(t *testing.T) {
	var homeDials atomic.Int64
	c := testConfig() // phone first
	phone := &r9Phone{status: statusAlways(403)}
	c.PhoneDialContext = phone.dial
	c.HomeDialContext = homeToOrigin(t, plainOrigin(t, "via-home"), &homeDials)
	r := newRelay(t, c)
	for i := 0; i < 2; i++ {
		if w := serve(r); w.Code != 200 || w.Body.String() != "via-home" {
			t.Fatalf("request %d: %d %q", i, w.Code, w.Body.String())
		}
	}
	// A phone that refuses the origin is healthy: it is tried again, never cooled down.
	if s := r.Snapshot(); s.Phone.Errors != 0 || !s.Phone.CooldownUntil.IsZero() || len(phone.seen()) != 2 || s.Phone.Active != 0 {
		t.Fatalf("%+v connects=%v", s, phone.seen())
	}
}

func TestClientStatusWhenNoPathServes(t *testing.T) {
	private := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.1")}, nil }
	dnsDown := func(context.Context, string) ([]net.IP, error) { return nil, errors.New("SERVFAIL") }
	for _, tc := range []struct {
		name   string
		lookup LookupIPFunc
		phone  int
		want   int
	}{
		{"homeRejectsPhoneRejects", private, 403, 403},
		{"phoneRejectsHomeDNSDown", dnsDown, 403, 403},
		{"homeRejectsPhoneDown", private, 502, 403},
		{"homeDNSDownPhoneDown", dnsDown, 502, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			c.LookupIP = tc.lookup
			phone := &r9Phone{status: statusAlways(tc.phone)}
			c.PhoneDialContext = phone.dial
			r := newRelay(t, c)
			if w := serve(r); w.Code != tc.want {
				t.Fatalf("%d %q", w.Code, w.Body.String())
			}
			if s := r.Snapshot(); s.Home.Active != 0 || s.Phone.Active != 0 {
				t.Fatalf("leaked active: %+v", s)
			}
		})
	}
}

func TestHomeMixedPrivateDNSRejectedWithoutDial(t *testing.T) {
	var homeDials atomic.Int64
	c := testConfig()
	c.Uplinks = UplinkHomeOnly
	c.LookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("192.168.1.1")}, nil
	}
	c.HomeDialContext = homeToOrigin(t, "127.0.0.1:1", &homeDials)
	r := newRelay(t, c)
	if w := serve(r); w.Code != 403 || homeDials.Load() != 0 {
		t.Fatalf("%d dials=%d", w.Code, homeDials.Load())
	}
}

func TestHomeBlackholedFirstIPStillReachesSecond(t *testing.T) {
	var dials atomic.Int64
	origin := plainOrigin(t, "second")
	c := testConfig()
	c.Uplinks = UplinkHomeOnly
	c.DialTimeout = 2 * time.Second
	c.LookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("8.8.8.8")}, nil
	}
	c.HomeDialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		dials.Add(1)
		if address == "1.1.1.1:80" {
			<-ctx.Done() // blackhole: only the per-candidate budget ends it
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin)
	}
	r := newRelay(t, c)
	start := time.Now()
	w := serve(r)
	if w.Code != 200 || w.Body.String() != "second" || dials.Load() != 2 {
		t.Fatalf("%d %q dials=%d", w.Code, w.Body.String(), dials.Load())
	}
	if took := time.Since(start); took >= c.DialTimeout {
		t.Fatalf("second candidate was reached only after the whole budget: %v", took)
	}
}

func TestHomeDialCandidateCapAndTotalBudget(t *testing.T) {
	answers := []net.IP{net.ParseIP("2606:4700:4700::1111"), net.ParseIP("8.8.8.8"), net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1"),
		net.ParseIP("9.9.9.9"), net.ParseIP("8.8.4.4"), net.ParseIP("208.67.222.222"), net.ParseIP("208.67.220.220")}
	t.Run("capIsFourDistinctIPv4", func(t *testing.T) {
		var mu sync.Mutex
		var dialled []string
		c := testConfig()
		c.Uplinks = UplinkHomeOnly
		c.LookupIP = func(context.Context, string) ([]net.IP, error) { return answers, nil }
		c.HomeDialContext = func(_ context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			dialled = append(dialled, network+" "+address)
			mu.Unlock()
			return nil, errors.New("refused")
		}
		r := newRelay(t, c)
		if w := serve(r); w.Code != 502 {
			t.Fatal(w.Code)
		}
		want := "tcp4 8.8.8.8:80,tcp4 1.1.1.1:80,tcp4 9.9.9.9:80,tcp4 8.8.4.4:80"
		if got := strings.Join(dialled, ","); got != want {
			t.Fatalf("dialled %s, want %s", got, want)
		}
	})
	t.Run("blackholeStaysInsideDialTimeout", func(t *testing.T) {
		var dials atomic.Int64
		c := testConfig()
		c.Uplinks = UplinkHomeOnly
		c.DialTimeout = 2 * time.Second
		c.LookupIP = func(context.Context, string) ([]net.IP, error) { return answers, nil }
		c.HomeDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			dials.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		r := newRelay(t, c)
		start := time.Now()
		if w := serve(r); w.Code != 502 {
			t.Fatal(w.Code)
		}
		if took := time.Since(start); took > c.DialTimeout+500*time.Millisecond || dials.Load() < 2 || dials.Load() > 4 {
			t.Fatalf("took %v with %d dials", took, dials.Load())
		}
	})
}

func TestRejectedResolveNotCountedAsAttempt(t *testing.T) {
	var private atomic.Bool
	private.Store(true)
	var homeDials atomic.Int64
	c := testConfig()
	c.Uplinks = UplinkHomeOnly
	c.LookupIP = func(context.Context, string) ([]net.IP, error) {
		if private.Load() {
			return []net.IP{net.ParseIP("10.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	c.HomeDialContext = homeToOrigin(t, plainOrigin(t, "ok"), &homeDials)
	r := newRelay(t, c)
	for i := 0; i < 3; i++ {
		if w := serve(r); w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if s := r.Snapshot(); s.Home.Requests != 0 || s.Home.Active != 0 || s.Home.Errors != 0 || !s.Home.CooldownUntil.IsZero() {
		t.Fatalf("rejected resolve was counted: %+v", s)
	}
	private.Store(false)
	if w := serve(r); w.Code != 200 {
		t.Fatalf("rejections cooled the path down: %d", w.Code)
	}
	// In dual mode the same rejection lets the phone try without counting the home.
	c2 := testConfig()
	c2.HomeWeight, c2.PhoneWeight = 4, 1
	c2.LookupIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.1")}, nil }
	phone := &r9Phone{status: statusAlways(200), origin: plainOrigin(t, "phone")}
	c2.PhoneDialContext = phone.dial
	r2 := newRelay(t, c2)
	if w := serve(r2); w.Code != 200 || w.Body.String() != "phone" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if s := r2.Snapshot(); s.Home.Requests != 0 || s.Home.Errors != 0 || s.Phone.Requests != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestTLSHomeDNSFailureUsesPhone(t *testing.T) {
	c := testConfig()
	c.HomeWeight, c.PhoneWeight = 4, 1 // home is scheduled first
	var lookups atomic.Int64
	c.LookupIP = func(context.Context, string) ([]net.IP, error) {
		lookups.Add(1)
		return nil, errors.New("SERVFAIL")
	}
	r, client, _, home, phone := tlsIntegration(t, c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "phone") }), nil)
	resp, err := client.Get("https://" + tlsTestHost + "/")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(got) != "phone" || home.Load() != 0 || phone.Load() != 1 || lookups.Load() != 1 {
		t.Fatalf("%q %v home=%d phone=%d lookups=%d", got, err, home.Load(), phone.Load(), lookups.Load())
	}
	waitBudget(t, func() bool { return r.Snapshot().Phone.Active == 0 })
	if s := r.Snapshot(); s.Home.Errors != 1 || s.Phone.Errors != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestTLSPhoneUsesHostnameCONNECT(t *testing.T) {
	c := testConfig()
	c.Uplinks = UplinkPhoneOnly
	c.LookupIP = func(context.Context, string) ([]net.IP, error) {
		t.Error("phone TLS path used the desktop resolver")
		return nil, errors.New("must not be called")
	}
	// tlsIntegration's CONNECT server rejects anything but tlsTestHost:443.
	_, client, _, home, phone := tlsIntegration(t, c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }), nil)
	resp, err := client.Get("https://" + tlsTestHost + "/")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(got) != "ok" || phone.Load() != 1 || home.Load() != 0 {
		t.Fatalf("%q %v home=%d phone=%d", got, err, home.Load(), phone.Load())
	}
}

func TestPhoneOnlyStaleReturns503NoHomeFallback(t *testing.T) {
	var homeDials, lookups atomic.Int64
	c := testConfig()
	c.Uplinks = UplinkPhoneOnly
	c.LookupIP = func(context.Context, string) ([]net.IP, error) {
		lookups.Add(1)
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	c.HomeDialContext = homeToOrigin(t, plainOrigin(t, "home"), &homeDials)
	phone := &r9Phone{status: statusAlways(407)}
	c.PhoneDialContext = phone.dial
	r := newRelay(t, c)
	for i := 0; i < 2; i++ {
		w := serve(r)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "stale") {
			t.Fatalf("request %d: %d %q", i, w.Code, w.Body.String())
		}
	}
	if homeDials.Load() != 0 || lookups.Load() != 0 || len(phone.seen()) != 1 || r.Snapshot().Home.Requests != 0 {
		t.Fatalf("home touched: dials=%d lookups=%d connects=%v", homeDials.Load(), lookups.Load(), phone.seen())
	}
}
