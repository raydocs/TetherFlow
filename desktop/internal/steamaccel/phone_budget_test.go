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

// Count socket lifetimes, not HTTP requests or CONNECT handshakes. The dial
// counter also includes calls that have not yet returned a connected socket.
type socketCount struct{ alive, peak, total atomic.Int64 }

func (s *socketCount) open() {
	n := s.alive.Add(1)
	s.total.Add(1)
	for old := s.peak.Load(); n > old; old = s.peak.Load() {
		if s.peak.CompareAndSwap(old, n) {
			break
		}
	}
}

type countedSocket struct {
	net.Conn
	count *socketCount
	once  sync.Once
	err   error
}

func (c *countedSocket) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close(); c.count.alive.Add(-1) })
	return c.err
}

type countedListener struct {
	net.Listener
	count *socketCount
}

func (l countedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.count.open()
	return &countedSocket{Conn: c, count: l.count}, nil
}

// A real TCP CONNECT proxy forwarding to a real keepalive HTTP origin. Both
// client sockets and accepted proxy sockets remain counted while idle.
func budgetRelay(t *testing.T, c Config, handler http.Handler) (*Relay, *socketCount, *socketCount) {
	t.Helper()
	origin := httptest.NewServer(handler)
	t.Cleanup(origin.Close)
	originAddr := strings.TrimPrefix(origin.URL, "http://")
	clientSockets, serverSockets := new(socketCount), new(socketCount)
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect || req.Host != "8.8.8.8:80" || req.Header.Get("Proxy-Authorization") != "Bearer test-secret" {
			t.Error("invalid CONNECT request")
			http.Error(w, "CONNECT required", 407)
			return
		}
		up, err := net.Dial("tcp", originAddr)
		if err != nil {
			http.Error(w, "origin unavailable", 502)
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
		if err := rw.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { io.Copy(up, rw); up.Close(); close(done) }()
		io.Copy(down, up)
		down.Close()
		<-done
	}))
	proxy.Listener = countedListener{Listener: proxy.Listener, count: serverSockets}
	proxy.Start()
	t.Cleanup(proxy.Close)
	c.PhoneProxyAddr = strings.TrimPrefix(proxy.URL, "http://")
	c.HomeDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", originAddr)
	}
	phoneDial := c.PhoneDialContext
	if phoneDial == nil {
		phoneDial = (&net.Dialer{}).DialContext
	}
	c.PhoneDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		clientSockets.open()
		conn, err := phoneDial(ctx, network, addr)
		if err != nil {
			if conn != nil {
				conn.Close()
			}
			clientSockets.alive.Add(-1)
			return nil, err
		}
		return &countedSocket{Conn: conn, count: clientSockets}, nil
	}
	r := newRelay(t, c)
	// The hijacked sockets must be gone before origin/proxy cleanup.
	t.Cleanup(func() {
		r.Close()
		waitBudget(t, func() bool { return clientSockets.alive.Load() == 0 && serverSockets.alive.Load() == 0 })
	})
	return r, clientSockets, serverSockets
}

func waitBudget(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("socket state did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func phoneUsed(r *Relay) int {
	r.phoneSockets.mu.Lock()
	defer r.phoneSockets.mu.Unlock()
	return r.phoneSockets.used
}

func requireHealthyPhone(t *testing.T, r *Relay) {
	t.Helper()
	s := r.Snapshot()
	if s.Phone.Errors != 0 || !s.Phone.CooldownUntil.IsZero() {
		t.Fatalf("local socket pressure poisoned phone health: %+v", s)
	}
}

func TestPhoneTotalSocketBudgetConcurrentAndHostRotation(t *testing.T) {
	for _, configured := range []int{0, 3} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			const count = 64 // Default weighted scheduling would assign >32 to phone.
			c := testConfig()
			c.MaxPhoneConnections = configured
			limit := configured
			if limit == 0 {
				limit = 32
			}
			entered := make(chan struct{}, count)
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			r, clients, servers := budgetRelay(t, c, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/hold" {
					entered <- struct{}{}
					select {
					case <-release:
					case <-req.Context().Done():
						return
					}
				}
				io.WriteString(w, "ok")
			}))
			var wg sync.WaitGroup
			for i := 0; i < count; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					req := localRequest("GET", "/hold")
					req.Host = fmt.Sprintf("warm%d.steamcontent.com", i)
					req.URL.Host = req.Host
					w := httptest.NewRecorder()
					r.ServeHTTP(w, req)
					if w.Code != 200 || w.Body.String() != "ok" {
						t.Errorf("request failed instead of home fallback: %d %q", w.Code, w.Body.String())
					}
				}(i)
			}
			for i := 0; i < count; i++ {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("64 concurrent requests did not reach an origin")
				}
			}
			if clients.alive.Load() != int64(limit) || servers.alive.Load() != int64(limit) || phoneUsed(r) != limit {
				t.Fatalf("expected full phone budget %d: client=%d server=%d reserved=%d", limit, clients.alive.Load(), servers.alive.Load(), phoneUsed(r))
			}
			if s := r.Snapshot(); s.Phone.Requests <= uint64(limit) || s.Home.Active != uint64(count-limit) {
				t.Fatalf("phone capacity fallback was not exercised: %+v", s)
			}
			requireHealthyPhone(t, r)
			once.Do(func() { close(release) })
			wg.Wait()
			before := r.Snapshot().Phone.Requests
			// Fill and replace per-host idle pools repeatedly without waiting for TTL.
			for i := 0; i < 96; i++ {
				req := localRequest("GET", "/rotate")
				req.Host = fmt.Sprintf("rotate%d.steamcontent.com", i)
				req.URL.Host = req.Host
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != 200 || w.Body.String() != "ok" {
					t.Fatalf("host rotation failed: %d %q", w.Code, w.Body.String())
				}
			}
			s := r.Snapshot()
			if s.Phone.Requests-before < 60 || clients.total.Load() <= int64(limit*2) {
				t.Fatalf("idle hosts starved phone: %+v, phone dials=%d", s, clients.total.Load())
			}
			// The proxy's cleanup of an old FIN may lag the next TCP accept. The
			// hard local invariant counts actual dialing/live sockets until Close
			// returns; server counters additionally prove full-load and idle cleanup.
			if clients.peak.Load() > int64(limit) || phoneUsed(r) > limit {
				t.Fatalf("socket budget %d exceeded: client peak=%d reserved=%d", limit, clients.peak.Load(), phoneUsed(r))
			}
			waitBudget(t, func() bool { return servers.alive.Load() <= int64(limit) })
			requireHealthyPhone(t, r)
			r.Close()
			waitBudget(t, func() bool { return clients.alive.Load() == 0 && servers.alive.Load() == 0 && phoneUsed(r) == 0 })
		})
	}
}

func TestPhoneKeepaliveReuseAndActiveCancellation(t *testing.T) {
	c := testConfig()
	c.MaxPhoneConnections = 2
	c.HomeWeight, c.PhoneWeight = 1, 1000
	entered := make(chan struct{}, 1)
	r, clients, servers := budgetRelay(t, c, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/hold" {
			entered <- struct{}{}
			<-req.Context().Done()
			return
		}
		io.WriteString(w, "ok")
	}))
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, localRequest("GET", "/reuse"))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if clients.total.Load() != 1 || clients.alive.Load() != 1 || servers.alive.Load() != 1 || phoneUsed(r) != 1 {
		t.Fatalf("keepalive not reused/accounted: dials=%d client=%d server=%d used=%d", clients.total.Load(), clients.alive.Load(), servers.alive.Load(), phoneUsed(r))
	}
	for _, closeRelay := range []bool{false, true} {
		req := localRequest("GET", "/hold")
		ctx, cancel := context.WithCancel(req.Context())
		done := make(chan struct{})
		go func() { defer close(done); r.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx)) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("active phone request did not start")
		}
		if closeRelay {
			r.Close()
		} else {
			cancel()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("cancel/Close left an active request")
		}
		cancel()
		waitBudget(t, func() bool { return phoneUsed(r) == 0 && clients.alive.Load() == 0 && servers.alive.Load() == 0 })
		requireHealthyPhone(t, r)
	}
}

func TestPhoneDetachedDialsReserveUntilTimeoutOrClose(t *testing.T) {
	for _, closeRelay := range []bool{false, true} {
		t.Run(fmt.Sprint(closeRelay), func(t *testing.T) {
			c := testConfig()
			c.MaxPhoneConnections = 2
			c.HomeWeight, c.PhoneWeight = 1, 1000
			c.DialTimeout = 2 * time.Second
			entered := make(chan struct{}, 2)
			c.PhoneDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				entered <- struct{}{}
				<-ctx.Done()
				// Even a seam returning both a connection and an error must release it.
				return conn, ctx.Err()
			}
			r, clients, servers := budgetRelay(t, c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
			// Do not use CloseIdleConnections here: it intentionally cancels orphan
			// dials. We want to prove the reservation survives detached cancellation.
			for i := 0; i < 2; i++ {
				req := localRequest("GET", "/detached")
				req.RequestURI = ""
				ctx, cancel := context.WithCancel(context.WithValue(req.Context(), targetKey{}, dialTarget{
					host: "cache.steamcontent.com", port: "80", ips: []net.IP{net.ParseIP("8.8.8.8")},
				}))
				done := make(chan struct{})
				go func() {
					defer close(done)
					resp, _ := r.paths[1].transport.RoundTrip(req.WithContext(ctx))
					if resp != nil {
						resp.Body.Close()
					}
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("detached dial did not start")
				}
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("request cancellation blocked on detached dial")
				}
			}
			waitBudget(t, func() bool { return servers.alive.Load() == 2 })
			if phoneUsed(r) != 2 || clients.alive.Load() != 2 {
				t.Fatal("detached dials released their reservations before exiting")
			}
			if closeRelay {
				r.Close()
			}
			waitBudget(t, func() bool { return phoneUsed(r) == 0 && clients.alive.Load() == 0 && servers.alive.Load() == 0 })
			if clients.peak.Load() > 2 || servers.peak.Load() > 2 {
				t.Fatal("detached socket budget exceeded")
			}
			requireHealthyPhone(t, r)
		})
	}
}

func TestPhoneBudgetFailuresAndExactlyOnceRelease(t *testing.T) {
	c := testConfig()
	c.MaxPhoneConnections = 1
	r := newRelay(t, c)
	boom := errors.New("dial failed")
	if _, err := r.connectPhone(context.Background(), func(context.Context, string, string) (net.Conn, error) { return nil, boom }, "8.8.8.8:80"); !errors.Is(err, boom) || phoneUsed(r) != 0 {
		t.Fatalf("dial failure leaked reservation: %v, used=%d", err, phoneUsed(r))
	}
	for _, response := range []string{"HTTP/1.1 407 Denied\r\n\r\n", "HTTP/1.1 200 OK\r\n\r\n"} {
		client, server := net.Pipe()
		done := make(chan struct{})
		closePeer := make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(closePeer) })
		go func() {
			defer close(done)
			defer server.Close()
			http.ReadRequest(bufio.NewReader(server))
			io.WriteString(server, response)
			if strings.Contains(response, "200") {
				<-closePeer
			}
		}()
		conn, err := r.connectPhone(context.Background(), func(context.Context, string, string) (net.Conn, error) { return client, nil }, "8.8.8.8:80")
		if strings.Contains(response, "200") {
			if err != nil {
				t.Fatal(err)
			}
			once.Do(func() { close(closePeer) })
			if _, err := conn.Read(make([]byte, 1)); err == nil {
				t.Fatal("missing peer EOF")
			}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); conn.Close() }()
			}
			wg.Wait()
		} else if err == nil {
			t.Fatal("rejected CONNECT succeeded")
		}
		<-done
		if phoneUsed(r) != 0 {
			t.Fatalf("error/repeated Close miscounted sockets: %d", phoneUsed(r))
		}
	}
	for _, limit := range []int{-1, 33} {
		c.MaxPhoneConnections = limit
		if other, err := New(c); err == nil {
			other.Close()
			t.Fatalf("invalid socket budget accepted: %d", limit)
		}
	}
}
