package usbmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	ForwardPort    uint16 = 8283
	MaxConnections        = 32
	IdleTimeout           = 30 * time.Second
	MaxLifetime           = 30 * time.Minute
	relayBuffer           = 32 << 10
)

type Forwarder struct {
	client          *Client
	device          Device
	mu              sync.Mutex
	started, closed bool
	listener        net.Listener
	cancel          context.CancelFunc
	active          map[net.Conn]struct{}
	// Fixed public limits; private durations permit focused lifecycle tests.
	idle, lifetime time.Duration
}

func New(client *Client, device Device) (*Forwarder, error) {
	if client == nil {
		return nil, errors.New("nil usbmux client")
	}
	if !validDevice(device) {
		return nil, ErrDeviceNotFound
	}
	device.Serial = normalizeSerial(device.Serial)
	return &Forwarder{client: client, device: device, active: make(map[net.Conn]struct{}), idle: IdleTimeout, lifetime: MaxLifetime}, nil
}
func loopbackAddress(a net.Addr) bool {
	addr, ok := a.(*net.TCPAddr)
	return ok && addr.IP != nil && addr.IP.IsLoopback() && addr.Zone == ""
}
func (f *Forwarder) add(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		c.Close()
		return false
	}
	f.active[c] = struct{}{}
	return true
}
func (f *Forwarder) remove(c net.Conn) { c.Close(); f.mu.Lock(); delete(f.active, c); f.mu.Unlock() }
func (f *Forwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.cancel != nil {
		f.cancel()
	}
	if f.listener != nil {
		f.listener.Close()
	}
	for c := range f.active {
		c.Close()
	}
	return nil
}
func (f *Forwarder) Serve(parent context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("nil usbmux listener")
	}
	f.mu.Lock()
	if f.started {
		f.mu.Unlock()
		listener.Close()
		return errors.New("usbmux Forwarder.Serve called more than once")
	}
	f.started = true
	if f.closed {
		f.mu.Unlock()
		listener.Close()
		return ErrClosed
	}
	if !loopbackAddress(listener.Addr()) {
		f.mu.Unlock()
		listener.Close()
		return fmt.Errorf("forwarder listener must bind numeric loopback TCP")
	}
	ctx, cancel := context.WithCancel(parent)
	f.cancel = cancel
	f.listener = listener
	f.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { f.Close() })
	var wg sync.WaitGroup
	defer func() { f.Close(); stop(); wg.Wait() }()
	slots := make(chan struct{}, MaxConnections)
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			if parent.Err() != nil {
				return parent.Err()
			}
			return nil
		}
		local, e := listener.Accept()
		if e != nil {
			<-slots
			if parent.Err() != nil {
				return parent.Err()
			}
			f.mu.Lock()
			closed := f.closed
			f.mu.Unlock()
			if closed {
				return nil
			}
			return e
		}
		if !loopbackAddress(local.RemoteAddr()) {
			local.Close()
			<-slots
			continue
		}
		if !f.add(local) {
			<-slots
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); defer func() { <-slots }(); f.serveConn(ctx, local) }()
	}
}
func (f *Forwarder) serveConn(parent context.Context, local net.Conn) {
	defer f.remove(local)
	ctx, cancel := context.WithTimeout(parent, f.lifetime)
	defer cancel()
	// Cancellation covers both a pending handshake and either blocked relay side.
	stopLocal := context.AfterFunc(ctx, func() { local.Close() })
	defer stopLocal()
	remote, e := f.client.Connect(ctx, f.device, ForwardPort)
	if e != nil {
		return
	}
	if !f.add(remote) {
		return
	}
	defer f.remove(remote)
	stopRemote := context.AfterFunc(ctx, func() { remote.Close() })
	defer stopRemote()
	deadline, _ := ctx.Deadline()
	relay(local, remote, f.idle, deadline)
}

type progressDeadline struct {
	mu   sync.Mutex
	a, b net.Conn
	idle time.Duration
	end  time.Time
}

func (p *progressDeadline) touch() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := time.Now().Add(p.idle)
	if p.end.Before(d) {
		d = p.end
	}
	if e := p.a.SetDeadline(d); e != nil {
		return e
	}
	return p.b.SetDeadline(d)
}
func pump(dst, src net.Conn, progress *progressDeadline) error {
	buffer := make([]byte, relayBuffer)
	for {
		n, readErr := src.Read(buffer)
		if n > 0 {
			if e := progress.touch(); e != nil {
				return e
			}
			remaining := buffer[:n]
			for len(remaining) > 0 {
				written, e := dst.Write(remaining)
				if written > 0 {
					remaining = remaining[written:]
					if pe := progress.touch(); pe != nil {
						return pe
					}
				}
				if e != nil {
					return e
				}
				if written == 0 {
					return io.ErrShortWrite
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if half, ok := dst.(interface{ CloseWrite() error }); ok {
					return half.CloseWrite()
				}
				return errors.New("relay transport lacks half-close")
			}
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}
func relay(a, b net.Conn, idle time.Duration, end time.Time) {
	progress := &progressDeadline{a: a, b: b, idle: idle, end: end}
	if progress.touch() != nil {
		return
	}
	results := make(chan error, 2)
	go func() { results <- pump(a, b, progress) }()
	go func() { results <- pump(b, a, progress) }()
	if e := <-results; e != nil {
		a.Close()
		b.Close()
	}
	<-results
}
