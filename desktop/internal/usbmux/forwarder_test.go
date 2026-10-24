package usbmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func startForwarder(t *testing.T, client *Client) (*Forwarder, net.Listener, <-chan error) {
	t.Helper()
	f, e := New(client, testDevice)
	if e != nil {
		t.Fatal(e)
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- f.Serve(context.Background(), l) }()
	t.Cleanup(func() {
		f.Close()
		select {
		case e := <-done:
			if e != nil {
				t.Errorf("Serve: %v", e)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return f, l, done
}
func dialForwarder(t *testing.T, l net.Listener) *net.TCPConn {
	t.Helper()
	c, e := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	conn := c.(*net.TCPConn)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn
}
func echoSession(c net.Conn) error {
	if e := replyList(c, deviceXML(7, "AB-CD-EF", "USB")); e != nil {
		return e
	}
	tag, e := expectConnect(c)
	if e != nil {
		return e
	}
	if e = writeFull(c, append(packet(resultXML(0), tag), []byte("PREFIX\x00")...)); e != nil {
		return e
	}
	if _, e = io.Copy(c, c); e != nil {
		return e
	}
	return writeFull(c, []byte("\x00FINAL"))
}
func TestForwarderMultiTwoMiBExactAndHalfClose(t *testing.T) {
	client, server := startFake(t, echoSession)
	_, l, _ := startForwarder(t, client)
	const count = 4
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		go func(seed int) {
			c, e := net.DialTimeout("tcp", l.Addr().String(), time.Second)
			if e != nil {
				results <- e
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			payload := make([]byte, 2<<20)
			for i := range payload {
				payload[i] = byte(i*31 + seed)
			}
			sent := make(chan error, 1)
			go func() {
				e := writeFull(c, payload)
				if e == nil {
					e = c.(*net.TCPConn).CloseWrite()
				}
				sent <- e
			}()
			got, readErr := io.ReadAll(c)
			writeErr := <-sent
			want := append([]byte("PREFIX\x00"), payload...)
			want = append(want, []byte("\x00FINAL")...)
			if readErr != nil || writeErr != nil || !bytes.Equal(got, want) {
				results <- fmt.Errorf("seed %d bytes=%d want=%d read=%v write=%v", seed, len(got), len(want), readErr, writeErr)
				return
			}
			results <- nil
		}(i)
	}
	for i := 0; i < count; i++ {
		if e := <-results; e != nil {
			t.Error(e)
		}
	}
	server.check(t)
}
func TestForwarderBudgetIncludesPendingDialsAndTeardown(t *testing.T) {
	var started, live, peak atomic.Int32
	dialStarted := make(chan struct{}, 64)
	client, e := NewClient(Config{Network: "tcp", Address: "127.0.0.1:27015", DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		n := live.Add(1)
		defer live.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		started.Add(1)
		dialStarted <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if e != nil {
		t.Fatal(e)
	}
	f, l, _ := startForwarder(t, client)
	var locals []net.Conn
	defer func() {
		for _, c := range locals {
			c.Close()
		}
	}()
	for i := 0; i < MaxConnections+8; i++ {
		locals = append(locals, dialForwarder(t, l))
	}
	for i := 0; i < MaxConnections; i++ {
		select {
		case <-dialStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("did not fill budget")
		}
	}
	select {
	case <-dialStarted:
		t.Fatal("exceeded budget")
	case <-time.After(60 * time.Millisecond):
	}
	if started.Load() != 32 || peak.Load() != 32 {
		t.Fatalf("started=%d peak=%d", started.Load(), peak.Load())
	}
	f.Close()
	deadline := time.After(time.Second)
	for live.Load() != 0 {
		select {
		case <-deadline:
			t.Fatal("pending dials did not cancel")
		case <-time.After(time.Millisecond):
		}
	}
}
func TestForwarderBudgetIncludesActiveStreams(t *testing.T) {
	arrived := make(chan struct{}, 64)
	client, _ := startFake(t, func(c net.Conn) error {
		if e := replyList(c, deviceXML(7, "abcdef", "USB")); e != nil {
			return e
		}
		tag, e := expectConnect(c)
		if e != nil {
			return e
		}
		if e = writeFull(c, packet(resultXML(0), tag)); e != nil {
			return e
		}
		arrived <- struct{}{}
		var b [1]byte
		_, _ = c.Read(b[:])
		return nil
	})
	_, l, _ := startForwarder(t, client)
	var locals []*net.TCPConn
	defer func() {
		for _, c := range locals {
			c.Close()
		}
	}()
	for i := 0; i < MaxConnections+1; i++ {
		locals = append(locals, dialForwarder(t, l))
	}
	for i := 0; i < MaxConnections; i++ {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatal("not enough sessions")
		}
	}
	select {
	case <-arrived:
		t.Fatal("accepted 33rd active connection")
	case <-time.After(50 * time.Millisecond):
	}
	locals[0].Close()
	select {
	case <-arrived:
	case <-time.After(time.Second):
		t.Fatal("budget not released")
	}
}
func TestForwarderCloseActiveAndParentCancel(t *testing.T) {
	for _, parentCancel := range []bool{false, true} {
		t.Run(fmt.Sprint(parentCancel), func(t *testing.T) {
			ready := make(chan struct{})
			closed := make(chan struct{})
			client, _ := startFake(t, func(c net.Conn) error {
				if e := replyList(c, deviceXML(7, "abcdef", "USB")); e != nil {
					return e
				}
				tag, e := expectConnect(c)
				if e != nil {
					return e
				}
				if e = writeFull(c, packet(resultXML(0), tag)); e != nil {
					return e
				}
				close(ready)
				var b [1]byte
				_, _ = c.Read(b[:])
				close(closed)
				return nil
			})
			f, e := New(client, testDevice)
			if e != nil {
				t.Fatal(e)
			}
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer f.Close()
			done := make(chan error, 1)
			go func() { done <- f.Serve(ctx, l) }()
			local := dialForwarder(t, l)
			defer local.Close()
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("not ready")
			}
			if parentCancel {
				cancel()
			} else {
				f.Close()
				f.Close()
			}
			select {
			case e := <-done:
				if parentCancel && !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
				if !parentCancel && e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("Serve teardown stuck")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("upstream not closed")
			}
			var b [1]byte
			if _, e := local.Read(b[:]); e == nil {
				t.Fatal("local not closed")
			}
		})
	}
}
func TestForwarderIdleLifetimeAndSharedProgress(t *testing.T) {
	for _, mode := range []string{"idle", "lifetime", "progress"} {
		t.Run(mode, func(t *testing.T) {
			ready := make(chan struct{})
			client, _ := startFake(t, func(c net.Conn) error {
				if e := replyList(c, deviceXML(7, "abcdef", "USB")); e != nil {
					return e
				}
				tag, e := expectConnect(c)
				if e != nil {
					return e
				}
				if e = writeFull(c, packet(resultXML(0), tag)); e != nil {
					return e
				}
				close(ready)
				var b [1]byte
				for {
					n, e := c.Read(b[:])
					if n > 0 {
						if _, e := c.Write(b[:n]); e != nil {
							return nil
						}
					}
					if e != nil {
						return nil
					}
				}
			})
			f, e := New(client, testDevice)
			if e != nil {
				t.Fatal(e)
			}
			f.idle = 100 * time.Millisecond
			f.lifetime = 5 * time.Second
			if mode == "lifetime" {
				f.idle = time.Second
				f.lifetime = 120 * time.Millisecond
			}
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { done <- f.Serve(context.Background(), l) }()
			defer func() {
				f.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("Serve stuck")
				}
			}()
			local := dialForwarder(t, l)
			defer local.Close()
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("not ready")
			}
			var b [1]byte
			if mode == "progress" {
				for i := 0; i < 10; i++ {
					time.Sleep(25 * time.Millisecond)
					if e := writeFull(local, []byte{byte(i)}); e != nil {
						t.Fatal(e)
					}
					if _, e := io.ReadFull(local, b[:]); e != nil || b[0] != byte(i) {
						t.Fatalf("lost progress %v", e)
					}
				}
				return
			}
			started := time.Now()
			_, e = local.Read(b[:])
			if e == nil {
				t.Fatal("timeout did not close")
			}
			if time.Since(started) > time.Second {
				t.Fatal("timeout not enforced")
			}
		})
	}
}

type addressListener struct {
	net.Listener
	addr   net.Addr
	remote net.Addr
	once   sync.Once
}

func (l *addressListener) Addr() net.Addr {
	if l.addr != nil {
		return l.addr
	}
	return l.Listener.Addr()
}

type addressConn struct {
	net.Conn
	remote net.Addr
}

func (c *addressConn) RemoteAddr() net.Addr { return c.remote }
func (l *addressListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	var remote net.Addr
	l.once.Do(func() { remote = l.remote })
	if remote != nil {
		return &addressConn{Conn: c, remote: remote}, nil
	}
	return c, nil
}
func TestForwarderRejectsNonloopbackListenerAndPeer(t *testing.T) {
	var dialed atomic.Int32
	client, e := NewClient(Config{Network: "tcp", Address: "127.0.0.1:27015", DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialed.Add(1)
		return nil, errors.New("expected dial failure")
	}})
	if e != nil {
		t.Fatal(e)
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	f, e := New(client, testDevice)
	if e != nil {
		t.Fatal(e)
	}
	bad := &addressListener{Listener: l, addr: &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 123}}
	if e = f.Serve(context.Background(), bad); e == nil {
		t.Fatal("accepted wildcard listener")
	}
	l, e = net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	wrapped := &addressListener{Listener: l, remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 123}}
	f, e = New(client, testDevice)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- f.Serve(context.Background(), wrapped) }()
	defer func() {
		f.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Serve stuck")
		}
	}()
	c := dialForwarder(t, l)
	var b [1]byte
	_, e = c.Read(b[:])
	c.Close()
	if e == nil || dialed.Load() != 0 {
		t.Fatalf("nonloopback peer not skipped: %v dials=%d", e, dialed.Load())
	}
	c = dialForwarder(t, l)
	_, e = c.Read(b[:])
	c.Close()
	if e == nil || dialed.Load() != 1 {
		t.Fatal("loopback peer not accepted")
	}
}
func TestForwarderCloseBeforeServeAndRepeatServe(t *testing.T) {
	client, e := NewClient(Config{})
	if e != nil {
		t.Fatal(e)
	}
	f, e := New(client, testDevice)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Serve(context.Background(), l); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	l, e = net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Serve(context.Background(), l); e == nil {
		t.Fatal("repeat Serve accepted")
	}
}

func TestForwarderBlockedWriteTeardown(t *testing.T) {
	sending := make(chan struct{})
	finished := make(chan struct{})
	client, _ := startFake(t, func(c net.Conn) error {
		defer close(finished)
		if e := replyList(c, deviceXML(7, "abcdef", "USB")); e != nil {
			return e
		}
		tag, e := expectConnect(c)
		if e != nil {
			return e
		}
		if e = writeFull(c, packet(resultXML(0), tag)); e != nil {
			return e
		}
		close(sending)
		buffer := make([]byte, 32<<10)
		for {
			if e = writeFull(c, buffer); e != nil {
				return nil
			}
		}
	})
	f, l, _ := startForwarder(t, client)
	local := dialForwarder(t, l)
	defer local.Close()
	local.SetReadBuffer(1024)
	select {
	case <-sending:
	case <-time.After(time.Second):
		t.Fatal("upstream not sending")
	}
	// No downstream reads: eventually the bounded relay must block on Write.
	time.Sleep(30 * time.Millisecond)
	f.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("backpressured upstream did not close")
	}
}

func TestForwarderDoesNotSwitchAfterDisconnect(t *testing.T) {
	var sessions atomic.Int32
	client, server := startFake(t, func(c net.Conn) error {
		id := uint32(7)
		if sessions.Add(1) > 1 {
			id = 8
		}
		if e := replyList(c, deviceXML(id, "abcdef", "USB")); e != nil {
			return e
		}
		if id == 8 {
			var b [1]byte
			n, _ := c.Read(b[:])
			if n != 0 {
				return errors.New("forwarder switched devices")
			}
			return nil
		}
		tag, e := expectConnect(c)
		if e != nil {
			return e
		}
		return writeFull(c, append(packet(resultXML(0), tag), []byte("first")...))
	})
	_, l, _ := startForwarder(t, client)
	first := dialForwarder(t, l)
	// CloseWrite ensures both half-closes complete after the upstream disconnect.
	if e := first.CloseWrite(); e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(first)
	first.Close()
	if e != nil || string(got) != "first" {
		t.Fatalf("%q %v", got, e)
	}
	second := dialForwarder(t, l)
	got, e = io.ReadAll(second)
	second.Close()
	if e != nil || len(got) != 0 {
		t.Fatalf("unexpected fallback: %q %v", got, e)
	}
	server.check(t)
}
