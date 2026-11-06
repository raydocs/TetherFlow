package steamaccel

import (
	"bufio"
	"context"
	"encoding/json"
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

const (
	tokOld = "0123456789abcdefghjkmnpqrstvwxyz" // valid Crockford32 (every symbol once)
	tokNew = "zyxwvtsrqpnmkhjgfedcba9876543210"
	// Grouped, upper case and padded: what a person types from the phone screen.
	tokNewTyped = "  ZYXW-VTSR-QPNM-KHJG-FEDC-BA98-7654-3210 \n"
)

// fakePhone is a CONNECT proxy behind a dial seam. It accepts only tokens for
// which accept returns true (407 otherwise) and then serves HTTP over the tunnel.
type fakePhone struct {
	accept  func(token string) bool
	handle  func(req *http.Request, w io.Writer)
	gate    chan struct{} // when non-nil, every CONNECT waits for it before answering
	entered chan struct{} // receives one value per CONNECT that reached the proxy
	eofs    atomic.Int64  // tunnels the client closed
	mu      sync.Mutex
	tokens  []string
}

func newFakePhone(accept func(string) bool) *fakePhone {
	return &fakePhone{accept: accept, entered: make(chan struct{}, 256)}
}

func (f *fakePhone) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

func (f *fakePhone) dial(context.Context, string, string) (net.Conn, error) {
	a, b := net.Pipe()
	go f.serve(b)
	return a, nil
}

func (f *fakePhone) serve(b net.Conn) {
	defer b.Close()
	br := bufio.NewReader(b)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	token := strings.TrimPrefix(req.Header.Get("Proxy-Authorization"), "Bearer ")
	f.mu.Lock()
	f.tokens = append(f.tokens, token)
	f.mu.Unlock()
	f.entered <- struct{}{}
	if f.gate != nil {
		<-f.gate
	}
	if !f.accept(token) {
		io.WriteString(b, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
		return
	}
	io.WriteString(b, "HTTP/1.1 200 Connection Established\r\n\r\n")
	for {
		r, err := http.ReadRequest(br)
		if err != nil {
			f.eofs.Add(1)
			return
		}
		if f.handle != nil {
			f.handle(r, b)
		} else {
			io.WriteString(b, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nphone")
		}
	}
}

func homeBody(body string, calls *atomic.Int64) http.RoundTripper {
	return roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return goodResponse(body), nil
	})
}

func serveGET(r *Relay) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, localRequest("GET", "/x"))
	return w
}

func phoneAuthConfig(f *fakePhone, home http.RoundTripper) Config {
	c := testConfig()
	c.PhoneToken = tokOld
	c.PhoneDialContext = f.dial
	c.HomeTransport = home
	return c
}

func settle(t *testing.T, r *Relay) {
	t.Helper()
	waitBudget(t, func() bool { s := r.Snapshot(); return s.Home.Active == 0 && s.Phone.Active == 0 })
}

func TestPhone407MarksStaleWithoutCooldown(t *testing.T) {
	f := newFakePhone(func(string) bool { return false })
	var home atomic.Int64
	r := newRelay(t, phoneAuthConfig(f, homeBody("home", &home)))
	w := serveGET(r)
	if w.Code != 200 || w.Body.String() != "home" {
		t.Fatalf("dual did not continue on home in the same request: %d %q", w.Code, w.Body.String())
	}
	settle(t, r)
	s := r.Snapshot()
	if s.Phone.Errors != 0 || !s.Phone.CooldownUntil.IsZero() || s.Phone.Active != 0 || phoneUsed(r) != 0 {
		t.Fatalf("a 407 must not be a path error or cooldown: %+v", s)
	}
	if a := s.PhoneAuth; !a.Stale || a.Generation != 1 || a.Rejections != 1 || a.StaleSince.IsZero() {
		t.Fatalf("407 did not mark the phone stale: %+v", a)
	}
	if s.Home.Errors != 0 || s.Home.Requests != 1 {
		t.Fatalf("home accounting changed: %+v", s.Home)
	}
}

func TestTLSPhone407FallsBackHomeMarksStale(t *testing.T) {
	f := newFakePhone(func(string) bool { return false })
	r, client, _, home, phone := tlsIntegration(t, testConfig(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "home") }), func(c *Config, _ string) {
		c.PhoneDialContext = f.dial
	})
	get := func() string {
		resp, err := client.Get("https://" + tlsTestHost + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := get(); got != "home" || home.Load() != 1 || phone.Load() != 0 {
		t.Fatalf("407 did not fall back to home: %q home=%d phone=%d", got, home.Load(), phone.Load())
	}
	settle(t, r)
	if s := r.Snapshot(); s.Phone.Errors != 0 || !s.Phone.CooldownUntil.IsZero() || !s.PhoneAuth.Stale || s.PhoneAuth.Rejections != 1 || phoneUsed(r) != 0 {
		t.Fatalf("TLS 407 accounting: %+v", s)
	}
	if got := get(); got != "home" || home.Load() != 2 || len(f.seen()) != 1 {
		t.Fatalf("stale phone was sent again over TLS: %q home=%d connects=%d", got, home.Load(), len(f.seen()))
	}
}

func TestPhoneStalePathNotDialedUntilRefresh(t *testing.T) {
	f := newFakePhone(func(tok string) bool { return tok == tokNew })
	var home atomic.Int64
	r := newRelay(t, phoneAuthConfig(f, homeBody("home", &home)))
	for i := 0; i < 6; i++ {
		if w := serveGET(r); w.Code != 200 || w.Body.String() != "home" {
			t.Fatalf("request %d: %d %q", i, w.Code, w.Body.String())
		}
		settle(t, r)
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("stale token sent %d times, want exactly the one that was rejected", n)
	}
	if home.Load() != 6 || r.Snapshot().Phone.Requests != 1 {
		t.Fatalf("scheduler kept choosing the stale phone: home=%d %+v", home.Load(), r.Snapshot())
	}
	if err := r.SetPhoneToken(tokNew); err != nil {
		t.Fatal(err)
	}
	if w := serveGET(r); w.Body.String() != "phone" {
		t.Fatalf("phone not scheduled after refresh: %q", w.Body.String())
	}
}

func TestPhoneOnlyStaleFailsClosed(t *testing.T) {
	f := newFakePhone(func(string) bool { return false })
	var home atomic.Int64
	c := phoneAuthConfig(f, homeBody("home", &home))
	c.Uplinks = UplinkPhoneOnly
	r := newRelay(t, c)
	for i := 0; i < 3; i++ {
		w := serveGET(r)
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), tokOld) {
			t.Fatalf("phone-only stale must be 503 without auth data: %d %q", w.Code, w.Body.String())
		}
		settle(t, r)
	}
	if s := r.Snapshot(); home.Load() != 0 || s.Home.Requests != 0 || len(f.seen()) != 1 || s.Phone.Errors != 0 || !s.PhoneAuth.Stale {
		t.Fatalf("phone-only used home or resent a stale token: home=%d connects=%d %+v", home.Load(), len(f.seen()), s)
	}
}

func TestTLSPhoneOnlyStaleFailsClosed(t *testing.T) {
	f := newFakePhone(func(string) bool { return false })
	c := testConfig()
	c.Uplinks = UplinkPhoneOnly
	r, client, _, home, _ := tlsIntegration(t, c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "home") }), func(c *Config, _ string) {
		c.PhoneDialContext = f.dial
	})
	for i := 0; i < 2; i++ {
		if resp, err := client.Get("https://" + tlsTestHost + "/"); err == nil {
			resp.Body.Close()
			t.Fatal("phone-only TLS succeeded with a stale phone")
		}
		settle(t, r)
	}
	if s := r.Snapshot(); home.Load() != 0 || s.Home.Requests != 0 || len(f.seen()) != 1 || !s.PhoneAuth.Stale || s.Phone.Errors != 0 {
		t.Fatalf("phone-only TLS fell back or resent: home=%d connects=%d %+v", home.Load(), len(f.seen()), s)
	}
}

func TestSetPhoneTokenAppliesToNextCONNECTAndClearsStale(t *testing.T) {
	f := newFakePhone(func(tok string) bool { return tok == tokNew })
	var home atomic.Int64
	r := newRelay(t, phoneAuthConfig(f, homeBody("home", &home)))
	if w := serveGET(r); w.Body.String() != "home" {
		t.Fatalf("first request: %q", w.Body.String())
	}
	settle(t, r)
	if !r.Snapshot().PhoneAuth.Stale {
		t.Fatal("not stale after 407")
	}
	if err := r.SetPhoneToken(tokNewTyped); err != nil {
		t.Fatal(err)
	}
	a := r.Snapshot().PhoneAuth
	if a.Stale || a.Generation != 2 || !a.StaleSince.IsZero() || a.Rejections != 1 {
		t.Fatalf("refresh did not clear stale cleanly: %+v", a)
	}
	if w := serveGET(r); w.Code != 200 || w.Body.String() != "phone" {
		t.Fatalf("refreshed token not used: %d %q", w.Code, w.Body.String())
	}
	settle(t, r)
	if got := f.seen(); len(got) != 2 || got[0] != tokOld || got[1] != tokNew {
		t.Fatalf("CONNECT tokens = %q, want old then the normalized new one", got)
	}
	if s := r.Snapshot(); s.PhoneAuth.Stale || s.Phone.Errors != 0 || home.Load() != 1 {
		t.Fatalf("after refresh: home=%d %+v", home.Load(), s)
	}
}

// A CONNECT sent with generation 1 is still in flight when generation 2 is
// installed. Its late 407 is about the OLD token and must not stale the new one.
func TestStale407FromOldGenerationIgnored(t *testing.T) {
	f := newFakePhone(func(string) bool { return false })
	f.gate = make(chan struct{})
	r := newRelay(t, phoneAuthConfig(f, nil))
	const inflight = 8
	var wg sync.WaitGroup
	errs := make(chan error, inflight)
	for i := 0; i < inflight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.connectPhone(context.Background(), f.dial, "8.8.8.8:80")
			errs <- err
		}()
	}
	for i := 0; i < inflight; i++ {
		<-f.entered // every CONNECT carries generation 1 and sits in the proxy
	}
	if err := r.SetPhoneToken(tokNew); err != nil {
		t.Fatal(err)
	}
	close(f.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil || strings.Contains(err.Error(), tokOld) {
			t.Fatalf("old-generation 407 result: %v", err)
		}
	}
	a := r.Snapshot().PhoneAuth
	if a.Stale || a.Generation != 2 || a.Rejections != 0 || !a.StaleSince.IsZero() {
		t.Fatalf("an old generation's 407 invalidated the fresher token: %+v", a)
	}
	// And a 407 for the CURRENT generation still does.
	if _, err := r.connectPhone(context.Background(), f.dial, "8.8.8.8:80"); err == nil {
		t.Fatal("rejected CONNECT succeeded")
	}
	if a := r.Snapshot().PhoneAuth; !a.Stale || a.Generation != 2 || a.Rejections != 1 {
		t.Fatalf("current-generation 407 ignored: %+v", a)
	}
}

// Hammer SetPhoneToken, 407s, Snapshot and the scheduler together for the race
// detector (-race). Asserts that the last install wins and counts every install.
func TestPhoneAuthGenerationRace(t *testing.T) {
	f := newFakePhone(func(string) bool { return false })
	var home atomic.Int64
	r := newRelay(t, phoneAuthConfig(f, homeBody("home", &home)))
	toks := []string{tokOld, tokNew}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				serveGET(r)
				r.Snapshot()
			}
		}()
	}
	for i := 0; i < 200; i++ {
		if err := r.SetPhoneToken(toks[i%2]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if err := r.SetPhoneToken(tokNew); err != nil {
		t.Fatal(err)
	}
	a := r.Snapshot().PhoneAuth
	if a.Stale || a.Generation != 202 {
		t.Fatalf("last install must win and clear stale: %+v", a)
	}
}

func TestSetPhoneTokenRejectsInvalidKeepsOld(t *testing.T) {
	f := newFakePhone(func(tok string) bool { return tok == tokOld })
	var home atomic.Int64
	r := newRelay(t, phoneAuthConfig(f, homeBody("home", &home)))
	before := r.Snapshot().PhoneAuth
	for _, bad := range []string{"", "short", tokNew + "0", strings.Repeat("g", 64), strings.Repeat("u", 32), "bad\r\nX: y" + tokNew[:20], strings.Repeat("é", 16)} {
		err := r.SetPhoneToken(bad)
		if err == nil {
			t.Fatalf("invalid token %q accepted", bad)
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Fatalf("error echoes the rejected input: %v", err)
		}
	}
	if after := r.Snapshot().PhoneAuth; after != before {
		t.Fatalf("a rejected token changed state: %+v -> %+v", before, after)
	}
	if w := serveGET(r); w.Body.String() != "phone" {
		t.Fatalf("old token no longer used: %q", w.Body.String())
	}
	// A rejected refresh must not clear stale either.
	r2 := newRelay(t, phoneAuthConfig(newFakePhone(func(string) bool { return false }), homeBody("home", &home)))
	serveGET(r2)
	settle(t, r2)
	if r2.SetPhoneToken("nope") == nil || !r2.Snapshot().PhoneAuth.Stale {
		t.Fatal("invalid token cleared stale")
	}
	hc := testConfig()
	hc.Uplinks, hc.PhoneToken = UplinkHomeOnly, ""
	if newRelay(t, hc).SetPhoneToken(tokNew) == nil {
		t.Fatal("home-only relay accepted a phone token")
	}
}

func TestSetPhoneTokenClosesIdleKeepsActive(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		f := newFakePhone(func(string) bool { return true })
		var home atomic.Int64
		r := newRelay(t, phoneAuthConfig(f, homeBody("home", &home)))
		if w := serveGET(r); w.Body.String() != "phone" {
			t.Fatalf("got %q", w.Body.String())
		}
		waitBudget(t, func() bool { return phoneUsed(r) == 1 })
		// The transport parks the socket just after the body ends; keep installing
		// tokens until it is idle and closed, or fail after the deadline.
		deadline := time.Now().Add(3 * time.Second)
		for f.eofs.Load() == 0 && time.Now().Before(deadline) {
			if err := r.SetPhoneToken(tokNew); err != nil {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if f.eofs.Load() != 1 {
			t.Fatal("idle phone socket survived a token change")
		}
		waitBudget(t, func() bool { return phoneUsed(r) == 0 })
	})
	t.Run("active", func(t *testing.T) {
		f := newFakePhone(func(string) bool { return true })
		started, release := make(chan struct{}), make(chan struct{})
		f.handle = func(_ *http.Request, w io.Writer) {
			io.WriteString(w, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n01234")
			close(started)
			<-release
			io.WriteString(w, "56789")
		}
		var home atomic.Int64
		r := newRelay(t, phoneAuthConfig(f, homeBody("home", &home)))
		var rec *httptest.ResponseRecorder
		done := make(chan struct{})
		go func() { rec = serveGET(r); close(done) }()
		<-started
		waitBudget(t, func() bool { return r.Snapshot().Phone.Bytes >= 5 })
		if err := r.SetPhoneToken(tokNew); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		if phoneUsed(r) != 1 || f.eofs.Load() != 0 {
			t.Fatalf("active transfer was interrupted by a token change: used=%d", phoneUsed(r))
		}
		close(release)
		<-done
		if rec.Code != 200 || rec.Body.String() != "0123456789" {
			t.Fatalf("active transfer broken: %d %q", rec.Code, rec.Body.String())
		}
	})
}

func TestNormalizePhoneToken(t *testing.T) {
	hex64 := strings.Repeat("0123456789abcdef", 4)
	ok := map[string]string{
		tokOld: tokOld,
		"0123-4567-89ab-cdef-ghjk-mnpq-rstv-wxyz":  tokOld,
		strings.ToUpper(tokOld):                    tokOld,
		"  " + tokOld + "\r\n":                     tokOld,
		"0123 4567\t89AB-CDEF ghjk mnpq-rstv wxyz": tokOld,
		hex64:                  hex64, // legacy 64-hex iOS token
		strings.ToUpper(hex64): hex64,
		hex64[:60] + "AB-CD":   hex64[:60] + "abcd",
	}
	for in, want := range ok {
		got, err := NormalizePhoneToken(in)
		if err != nil || got != want {
			t.Errorf("NormalizePhoneToken(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", " - ", tokOld[:31], tokOld + "0", hex64[:63], hex64 + "0", // lengths
		"i123456789abcdefghjkmnpqrstvwxyz", "l123456789abcdefghjkmnpqrstvwxyz", // Crockford aliases are not decoded
		"o123456789abcdefghjkmnpqrstvwxyz", "u123456789abcdefghjkmnpqrstvwxyz",
		strings.Repeat("g", 64), strings.Repeat("a", 31) + "!", strings.Repeat("a", 31) + "é",
		strings.Repeat("a", 31) + "\x00", strings.Repeat("a", 5000), "test-secret",
	}
	for _, in := range bad {
		if got, err := NormalizePhoneToken(in); err == nil {
			t.Errorf("NormalizePhoneToken(%.40q) accepted as %q", in, got)
		} else if in != "" && len(in) > 3 && strings.Contains(err.Error(), in) {
			t.Errorf("error echoes input: %v", err)
		}
	}
}

func TestStatusNeverContainsToken(t *testing.T) {
	secret := "9z8y7x6w5v4t3s2r1q0pnmkhjgfedcba"
	f := newFakePhone(func(string) bool { return false })
	var home atomic.Int64
	c := phoneAuthConfig(f, homeBody("home", &home))
	c.PhoneToken = secret
	r := newRelay(t, c)
	var seen []string
	_, err := r.connectPhone(context.Background(), f.dial, "8.8.8.8:80")
	seen = append(seen, fmt.Sprint(err))
	serveGET(r)
	settle(t, r)
	phoneOnly := c
	phoneOnly.Uplinks = UplinkPhoneOnly
	r2 := newRelay(t, phoneOnly)
	serveGET(r2) // marks stale
	settle(t, r2)
	w := serveGET(r2)
	seen = append(seen, w.Body.String(), fmt.Sprint(r.SetPhoneToken(secret[:31])))
	for _, s := range []Status{r.Snapshot(), r2.Snapshot()} {
		j, _ := json.Marshal(s)
		seen = append(seen, fmt.Sprintf("%v|%+v|%#v", s, s, s), string(j))
	}
	seen = append(seen, fmt.Sprintf("%+v %#v", r.auth.status(), r.Snapshot().PhoneAuth))
	for _, s := range seen {
		if strings.Contains(s, secret) || strings.Contains(s, secret[:12]) {
			t.Fatalf("token material in output: %q", s)
		}
	}
}
