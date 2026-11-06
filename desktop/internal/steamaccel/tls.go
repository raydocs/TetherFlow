package steamaccel

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const maxClientHello = 64 << 10

var errHelloCaptured = errors.New("ClientHello captured; TLS termination forbidden")
var errNoHalfClose = errors.New("connection does not support write half-close")

type tlsIngress struct {
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
}

// ServeTLS forwards opaque TLS to an allowlisted visible SNI on numeric pinned
// port 443. Caller binds exact HomeIPv4:443. It owns the listener; listener exit
// or Relay.Close closes and waits for all its sessions. It never terminates TLS.
func (r *Relay) ServeTLS(listener net.Listener) error {
	if listener == nil {
		return errors.New("nil TLS listener")
	}
	ctx, cancel := context.WithCancel(r.ctx)
	ingress := &tlsIngress{listener: listener, cancel: cancel, done: make(chan struct{})}
	r.tlsMu.Lock()
	if r.ctx.Err() != nil {
		r.tlsMu.Unlock()
		cancel()
		listener.Close()
		return net.ErrClosed
	}
	r.tlsServers[ingress] = struct{}{}
	r.tlsMu.Unlock()
	var sessions sync.WaitGroup
	defer func() {
		cancel()
		listener.Close()
		sessions.Wait()
		r.tlsMu.Lock()
		delete(r.tlsServers, ingress)
		close(ingress.done)
		r.tlsMu.Unlock()
	}()
	for {
		client, err := listener.Accept()
		if err != nil {
			return err
		}
		if ctx.Err() != nil || !r.peerAllowed(client.RemoteAddr().String()) {
			client.Close()
			continue
		}
		select {
		case r.slots <- struct{}{}:
		default:
			client.Close()
			continue
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			defer func() { <-r.slots }()
			defer client.Close()
			r.serveTLS(ctx, client)
		}()
	}
}

func (r *Relay) closeTLS() {
	r.tlsMu.Lock()
	servers := make([]*tlsIngress, 0, len(r.tlsServers))
	for s := range r.tlsServers {
		servers = append(servers, s)
	}
	r.tlsMu.Unlock()
	for _, s := range servers {
		s.cancel()
		s.listener.Close()
	}
	for _, s := range servers {
		<-s.done
	}
}

// captureHelloConn is deliberately unable to write even an error alert to the
// client. crypto/tls parses/validates a ClientHello but never runs a handshake.
type captureHelloConn struct {
	net.Conn
	wire []byte
}

func (c *captureHelloConn) Read(p []byte) (int, error) {
	remaining := maxClientHello - len(c.wire)
	if remaining <= 0 {
		return 0, errors.New("ClientHello exceeds 64 KiB")
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := c.Conn.Read(p)
	c.wire = append(c.wire, p[:n]...)
	return n, err
}
func (c *captureHelloConn) Write([]byte) (int, error) { return 0, errHelloCaptured }

func readClientHello(ctx context.Context, conn net.Conn) (string, []byte, error) {
	capture := &captureHelloConn{Conn: conn}
	var name string
	inspected := false
	parser := tls.Server(capture, &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		inspected = true
		name = hello.ServerName
		return nil, errHelloCaptured
	}})
	err := parser.HandshakeContext(ctx)
	if !inspected || !errors.Is(err, errHelloCaptured) {
		return "", nil, errors.New("invalid or incomplete TLS ClientHello")
	}
	if name == "" || helloHasECH(capture.wire) {
		return "", nil, errors.New("visible SNI required; ECH unsupported")
	}
	return name, capture.wire, nil
}

// Inspect only the already crypto/tls-validated first ClientHello. Reassemble
// record payloads, since a handshake can span multiple TLS records. Reject ECH
// even if an outer cover name is allowlisted: we cannot recover the inner SNI.
func helloHasECH(wire []byte) bool {
	var hello []byte
	for len(wire) >= 5 {
		n := int(binary.BigEndian.Uint16(wire[3:5]))
		if n > len(wire)-5 || wire[0] != 22 {
			return true
		}
		hello = append(hello, wire[5:5+n]...)
		wire = wire[5+n:]
		if len(hello) >= 4 {
			size := int(hello[1])<<16 | int(hello[2])<<8 | int(hello[3])
			if len(hello) >= 4+size {
				hello = hello[4 : 4+size]
				break
			}
		}
	}
	if len(hello) < 35 {
		return true
	}
	rest := hello[34:]
	// session ID, cipher suites, compression methods, extensions.
	for _, width := range []int{1, 2, 1} {
		if len(rest) < width {
			return true
		}
		n := int(rest[0])
		if width == 2 {
			n = int(binary.BigEndian.Uint16(rest))
		}
		if n > len(rest)-width {
			return true
		}
		rest = rest[width+n:]
	}
	if len(rest) == 0 {
		return false
	}
	if len(rest) < 2 || int(binary.BigEndian.Uint16(rest)) != len(rest)-2 {
		return true
	}
	for rest = rest[2:]; len(rest) > 0; {
		if len(rest) < 4 {
			return true
		}
		kind, n := binary.BigEndian.Uint16(rest), int(binary.BigEndian.Uint16(rest[2:]))
		if kind == 0xfe0d || n > len(rest)-4 {
			return true
		}
		rest = rest[4+n:]
	}
	return false
}

func (r *Relay) serveTLS(parent context.Context, client net.Conn) {
	ctx, cancel := context.WithTimeout(parent, r.cfg.RequestTimeout)
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer func() { stop(); cancel() }()
	deadline := time.Now().Add(r.cfg.HeaderTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if client.SetDeadline(deadline) != nil {
		return
	}
	name, hello, err := readClientHello(ctx, client)
	if err != nil {
		return
	}
	host, _, err := r.authority(net.JoinHostPort(name, "443"))
	if err != nil {
		return
	}
	lookupCtx, lookupCancel := context.WithTimeout(ctx, r.cfg.DialTimeout)
	ips, err := r.cfg.LookupIP(lookupCtx, host)
	lookupCancel()
	if err != nil || len(ips) == 0 || len(ips) > 64 {
		return
	}
	pinned := make([]net.IP, len(ips))
	for i, ip := range ips {
		if !publicIP(ip) {
			return
		}
		pinned[i] = append(net.IP(nil), ip...)
	}
	ctx = context.WithValue(ctx, targetKey{}, dialTarget{host: host, port: "443", ips: pinned})
	previous := -1
	for attempt := 0; attempt < 2 && ctx.Err() == nil; attempt++ {
		i := r.acquire(previous)
		if i < 0 {
			return
		}
		dial, phone := r.homeDial, i == 1
		if phone {
			dial = r.phoneDial
		}
		upstream, err := r.originDial(dial, phone)(ctx, "tcp", net.JoinHostPort(host, "443"))
		sent := 0
		if err == nil {
			stopUp := context.AfterFunc(ctx, func() { upstream.Close() })
			if err = upstream.SetWriteDeadline(time.Now().Add(r.cfg.DialTimeout)); err == nil {
				sent, err = writeHello(upstream, hello)
			}
			if err == nil {
				outcome := r.pipeTLS(ctx, client, upstream, i)
				stopUp()
				upstream.Close()
				r.finish(i, outcome.pathFailure(), 0)
				return
			}
			stopUp()
			upstream.Close()
		}
		localCapacity := errors.Is(err, errDialBusy) || errors.Is(err, errPhoneCapacity)
		r.finish(i, ctx.Err() == nil && !localCapacity, 0)
		if sent != 0 || errors.Is(err, errDialBusy) {
			return
		}
		previous = i
	}
}

func writeHello(w io.Writer, hello []byte) (int, error) {
	total := 0
	for total < len(hello) {
		n, err := w.Write(hello[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func closeWrite(conn net.Conn) error {
	if c, ok := conn.(interface{ CloseWrite() error }); ok {
		return c.CloseWrite()
	}
	return errNoHalfClose
}

// tlsCause is why a spliced session ended. Only the first cause counts, and it is
// recorded before either socket is closed so our own Close cannot mask it.
type tlsCause int

const (
	causeUnset       tlsCause = iota
	causeClean                // orderly end, or idle after downstream progress (keep-alive)
	causeClient               // client read/write error or reset
	causeUpstream             // upstream read/write error, or EOF before any downstream byte
	causeUploadStall          // idle expiry whose last progress was client->upstream
	causeCancelled            // listener, relay or request-lifetime cancellation
)

type tlsOutcome struct {
	bytes uint64
	cause tlsCause
}

// pathFailure reports whether the cause is the uplink's fault.
func (o tlsOutcome) pathFailure() bool {
	return o.cause == causeUpstream || o.cause == causeUploadStall
}

type tlsFirstCause struct {
	mu    sync.Mutex
	cause tlsCause
}

func (f *tlsFirstCause) set(c tlsCause) {
	f.mu.Lock()
	if f.cause == causeUnset {
		f.cause = c
	}
	f.mu.Unlock()
}
func (f *tlsFirstCause) get() tlsCause {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cause
}

// Activity in either direction keeps a download alive: a quiet upload direction
// must not time out a continuously progressing download. A shared idle timer
// closes both sockets when neither direction makes progress. It remembers which
// direction progressed last so expiry can tell a stalled upload from keep-alive.
type tlsActivity struct {
	mu               sync.Mutex
	timer            *time.Timer
	last             time.Time
	lastUpload       bool
	timeout          time.Duration
	closed           bool
	client, upstream net.Conn
	onIdle           func(lastUpload bool)
}

func newTLSActivity(client, upstream net.Conn, timeout time.Duration, onIdle func(bool)) *tlsActivity {
	// The ClientHello already written upstream counts as upload progress.
	a := &tlsActivity{last: time.Now(), lastUpload: true, timeout: timeout, client: client, upstream: upstream, onIdle: onIdle}
	a.timer = time.AfterFunc(timeout, a.expire)
	return a
}
func (a *tlsActivity) expire() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	if remaining := a.timeout - time.Since(a.last); remaining > 0 {
		a.timer.Reset(remaining)
		a.mu.Unlock()
		return
	}
	a.closed = true
	upload := a.lastUpload
	a.mu.Unlock()
	a.onIdle(upload) // record the cause before closing anything
	a.client.Close()
	a.upstream.Close()
}
func (a *tlsActivity) touch(upload bool) {
	a.mu.Lock()
	if !a.closed {
		a.last = time.Now()
		a.lastUpload = upload
	}
	a.mu.Unlock()
}
func (a *tlsActivity) stop() {
	a.mu.Lock()
	a.closed = true
	a.timer.Stop()
	a.mu.Unlock()
}

type tlsPipe struct {
	ctx   context.Context
	first tlsFirstCause
	down  atomic.Uint64
}

// fail records a cause; once the session context is done every later error is a
// consequence of cancellation, never the path's fault.
func (p *tlsPipe) fail(c tlsCause) {
	if p.ctx.Err() != nil {
		c = causeCancelled
	}
	p.first.set(c)
}

// Hiding ReaderFrom/WriterTo guarantees one fixed copy buffer per direction.
// fromClient marks the client socket: reading it or writing the upstream is
// upload progress, the reverse is download progress.
type activeTLSConn struct {
	net.Conn
	activity *tlsActivity
	pipe     *tlsPipe
	client   bool
}

func (c activeTLSConn) side() tlsCause {
	if c.client {
		return causeClient
	}
	return causeUpstream
}

func (c activeTLSConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.activity.touch(c.client)
	}
	if err == io.EOF {
		if !c.client && c.pipe.down.Load() == 0 {
			c.pipe.fail(causeUpstream) // upstream closed without answering
		}
	} else if err != nil {
		c.pipe.fail(c.side())
	}
	return n, err
}
func (c activeTLSConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.activity.touch(!c.client)
	}
	if err != nil {
		c.pipe.fail(c.side())
	}
	return n, err
}

type countedTLSWriter struct {
	activeTLSConn
	relay *Relay
	path  int
}

func (w countedTLSWriter) Write(p []byte) (int, error) {
	n, err := w.activeTLSConn.Write(p)
	if n > 0 {
		w.pipe.down.Add(uint64(n))
		w.relay.mu.Lock()
		w.relay.paths[w.path].status.Bytes += uint64(n)
		w.relay.mu.Unlock()
	}
	return n, err
}

func (r *Relay) pipeTLS(ctx context.Context, client, upstream net.Conn, path int) tlsOutcome {
	pipe := &tlsPipe{ctx: ctx}
	if client.SetDeadline(time.Time{}) != nil || upstream.SetDeadline(time.Time{}) != nil {
		pipe.fail(causeClient)
		client.Close()
		upstream.Close()
		return tlsOutcome{cause: pipe.first.get()}
	}
	activity := newTLSActivity(client, upstream, r.cfg.IdleTimeout, func(lastUpload bool) {
		if lastUpload {
			pipe.fail(causeUploadStall)
		} else {
			pipe.fail(causeClean) // idle after downstream progress is keep-alive
		}
	})
	defer activity.stop()
	stop := context.AfterFunc(ctx, func() { pipe.first.set(causeCancelled); client.Close(); upstream.Close() })
	defer stop()
	c := activeTLSConn{client, activity, pipe, true}
	u := activeTLSConn{upstream, activity, pipe, false}
	upload := make(chan struct{}, 1)
	go func() {
		_, err := io.CopyBuffer(u, c, make([]byte, 32<<10))
		if err == nil {
			if err = closeWrite(upstream); err != nil {
				if errors.Is(err, errNoHalfClose) {
					pipe.fail(causeClean)
				} else {
					pipe.fail(causeUpstream)
				}
			}
		}
		if err != nil {
			client.Close()
			upstream.Close()
		}
		upload <- struct{}{}
	}()
	n, err := io.CopyBuffer(countedTLSWriter{c, r, path}, u, make([]byte, 32<<10))
	if err == nil {
		if err = closeWrite(client); err != nil {
			if errors.Is(err, errNoHalfClose) {
				pipe.fail(causeClean)
			} else {
				pipe.fail(causeClient)
			}
		}
	}
	// An upstream that closed without answering is a failed path: do not hold the
	// slot waiting for the client to hang up.
	if err != nil || ctx.Err() != nil || pipe.first.get() == causeUpstream {
		client.Close()
		upstream.Close()
	}
	<-upload
	pipe.fail(causeClean) // no-op unless nothing was recorded
	return tlsOutcome{bytes: uint64(n), cause: pipe.first.get()}
}
