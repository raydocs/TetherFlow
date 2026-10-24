package steamaccel

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCONNECTBearerAndPreservedBufferedPayload(t *testing.T) {
	for _, port := range []string{"80", "443"} {
		t.Run(port, func(t *testing.T) {
			c := testConfig()
			r := newRelay(t, c)
			client, server := net.Pipe()
			done := make(chan struct{})
			payload := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")
			go func() {
				defer close(done)
				defer server.Close()
				req, err := http.ReadRequest(bufio.NewReader(server))
				if err != nil {
					t.Error(err)
					return
				}
				if req.Method != "CONNECT" || req.Host != "8.8.8.8:"+port || req.RequestURI != "8.8.8.8:"+port || req.Header.Get("Proxy-Authorization") != "Bearer test-secret" {
					t.Errorf("CONNECT changed: %+v", req)
					return
				}
				// A single write makes tunnel data available in the same read as CONNECT headers.
				server.Write(append([]byte("HTTP/1.1 200 Connection Established\r\nX-Proxy: ok\r\n\r\n"), payload...))
				io.Copy(io.Discard, server)
			}()
			dial := func(_ context.Context, network, address string) (net.Conn, error) {
				if network != "tcp" || address != "127.0.0.1:8283" {
					t.Errorf("wrong proxy dial: %s %s", network, address)
				}
				return client, nil
			}
			conn, err := r.connectPhone(context.Background(), dial, "8.8.8.8:"+port)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(payload))
			_, err = io.ReadFull(conn, got)
			conn.Close()
			<-done
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("buffered tunnel bytes lost: %q", got)
			}
		})
	}
}

func TestCONNECTRejectsBadAndUnboundedHeaders(t *testing.T) {
	responses := []string{"HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n", "HTTP/1.1 201 Created\r\n\r\n", "NOT HTTP\r\n\r\n", "HTTP/2.0 200 OK\r\n\r\n", "HTTP/1.1 200 OK\r\nX-Long: " + strings.Repeat("x", 40000), "HTTP/1.1 200 OK\r\nX-Bad\r\n\r\n"}
	for i, response := range responses {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			c := testConfig()
			r := newRelay(t, c)
			client, server := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer server.Close()
				if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
					t.Error(err)
					return
				}
				server.Write([]byte(response))
			}()
			conn, err := r.connectPhone(context.Background(), func(context.Context, string, string) (net.Conn, error) { return client, nil }, "8.8.8.8:80")
			if err == nil {
				conn.Close()
				t.Fatal("bad CONNECT accepted")
			}
			<-done
			if strings.Contains(err.Error(), c.PhoneToken) {
				t.Error("token leaked in error")
			}
		})
	}
}

func TestCONNECTCancellationClosesBlockedSocket(t *testing.T) {
	c := testConfig()
	r := newRelay(t, c)
	client, server := net.Pipe()
	entered := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer server.Close()
		http.ReadRequest(bufio.NewReader(server))
		close(entered)
		io.Copy(io.Discard, server)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.connectPhone(ctx, func(context.Context, string, string) (net.Conn, error) { return client, nil }, "8.8.8.8:80")
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("CONNECT cancellation stalled")
	}
	<-serverDone
}

func TestCONNECTHeaderTimeout(t *testing.T) {
	c := testConfig()
	c.HeaderTimeout = 20 * time.Millisecond
	r := newRelay(t, c)
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan struct{})
	go func() { defer close(done); http.ReadRequest(bufio.NewReader(server)); io.Copy(io.Discard, server) }()
	_, err := r.connectPhone(context.Background(), func(context.Context, string, string) (net.Conn, error) { return client, nil }, "8.8.8.8:80")
	if err == nil {
		t.Fatal("missing header timeout")
	}
	<-done
}
