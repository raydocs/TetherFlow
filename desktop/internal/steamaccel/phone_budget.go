package steamaccel

import (
	"context"
	"errors"
	"net"
	"sync"
)

var errPhoneCapacity = errors.New("local phone socket capacity exhausted")

// A reservation starts BEFORE dialing and lasts until the socket is closed, not
// just until a request or CONNECT handshake finishes. Detached transport dials
// and all per-host keepalive pools share the same budget.
type phoneBudget struct {
	mu     sync.Mutex
	limit  int
	used   int
	closed bool
	conns  map[*phoneConn]struct{}
}

func (b *phoneBudget) reserve() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return context.Canceled
	}
	if b.used >= b.limit {
		return errPhoneCapacity
	}
	b.used++
	return nil
}

func (b *phoneBudget) release(c *phoneConn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.conns, c)
	b.used--
}

// adopt transfers a dial reservation to a socket. Close racing with a detached
// dial cannot admit that socket after shutdown, nor release its slot early.
func (b *phoneBudget) adopt(conn net.Conn) (net.Conn, error) {
	c := &phoneConn{Conn: conn, budget: b}
	b.mu.Lock()
	closed := b.closed
	if !closed {
		b.conns[c] = struct{}{}
	}
	b.mu.Unlock()
	if closed {
		c.Close()
		return nil, context.Canceled
	}
	return c, nil
}

func (b *phoneBudget) close() {
	b.mu.Lock()
	b.closed = true
	conns := make([]*phoneConn, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

type phoneConn struct {
	net.Conn
	budget *phoneBudget
	once   sync.Once
	err    error
}

func (c *phoneConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.budget.release(c)
	})
	return c.err
}

// EOF closes only the peer's write direction. Keep the socket and its budget
// reservation alive until explicit Close; the other direction may still be used.
func (c *phoneConn) CloseWrite() error { return closeWrite(c.Conn) }

func (r *Relay) reservePhone() error {
	err := r.phoneSockets.reserve()
	if !errors.Is(err, errPhoneCapacity) {
		return err
	}
	// Transport reuses a matching idle socket before dialing. At the hard limit,
	// evict idle sockets (possibly for old CDN hosts), never active transfers.
	// Keep keepalive enabled on the normal path; don't make rotation wait for TTL.
	if t, ok := r.paths[1].transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
	return r.phoneSockets.reserve()
}
