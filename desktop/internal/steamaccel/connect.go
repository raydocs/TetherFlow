package steamaccel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (r *Relay) connectPhone(ctx context.Context, dial DialContextFunc, target string) (_ net.Conn, err error) {
	ctx, cancel := context.WithCancel(ctx)
	stopRelay := context.AfterFunc(r.ctx, cancel)
	defer func() { stopRelay(); cancel() }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.reservePhone(); err != nil {
		return nil, err
	}
	conn, err := dial(ctx, "tcp", r.cfg.PhoneProxyAddr)
	if err != nil {
		// A failing trusted dial seam may still return a socket.
		if conn != nil {
			conn.Close()
		}
		r.phoneSockets.release(nil)
		return nil, err
	}
	conn, err = r.phoneSockets.adopt(conn)
	if err != nil {
		return nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			conn.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(r.cfg.HeaderTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nProxy-Authorization: Bearer " + r.cfg.PhoneToken + "\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 4096)
	header := make([]byte, 0, 4096)
	// ReadSlice bounds each allocation even if a hostile proxy never sends CRLF.
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(header)+len(fragment) > r.cfg.MaxHeaderBytes {
			return nil, errors.New("CONNECT response headers too large")
		}
		header = append(header, fragment...)
		if readErr != nil && readErr != bufio.ErrBufferFull {
			return nil, fmt.Errorf("CONNECT response: %w", readErr)
		}
		if readErr == nil && len(header) >= 4 && bytes.Equal(header[len(header)-4:], []byte("\r\n\r\n")) {
			break
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header)), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, fmt.Errorf("invalid CONNECT response: %w", err)
	}
	if resp.ProtoMajor != 1 || resp.ProtoMinor > 1 || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CONNECT rejected with status %d", resp.StatusCode)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Stop the cancellation callback before returning ownership to http.Transport.
	if !stop() {
		return nil, ctx.Err()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	succeeded = true
	return &bufferedConn{Conn: conn, reader: reader}, nil
}
