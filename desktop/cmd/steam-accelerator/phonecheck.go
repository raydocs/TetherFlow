package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// phoneProtocol is the only /diagnostic/info protocol this build speaks: 2 is the phone
// that validates the connected peer (a public address) before it sends CONNECT 200.
const phoneProtocol = 2

const (
	stageInfo    = "info"
	stageUSBLoop = "usb-loop"
	stageConnect = "connect"
	stageTLS     = "tls"
)

// phoneCheckError is a failed phone preflight step. Stage says which step failed and
// Status is the phone's HTTP status when it answered (0 otherwise). Callers branch on
// these fields, never on message text. The message never holds a token.
type phoneCheckError struct {
	Stage  string
	Status int
	Err    error
}

func (e *phoneCheckError) Error() string { return "phone " + e.Stage + " check: " + e.Err.Error() }
func (e *phoneCheckError) Unwrap() error { return e.Err }

func stageErr(stage string, status int, err error) error {
	return &phoneCheckError{Stage: stage, Status: status, Err: err}
}

func loopbackAddr(addr string) error {
	host, port, e := net.SplitHostPort(addr)
	if e != nil {
		return e
	}
	ip := net.ParseIP(host)
	p, e := strconv.Atoi(port)
	if e != nil || p < 1 || p > 65535 || ip == nil || !ip.IsLoopback() {
		return errors.New("phone endpoint must be numeric loopback")
	}
	return nil
}

func phoneHTTPClient() (*http.Client, func()) {
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 * 1024}
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("unexpected phone redirect") }}
	return client, tr.CloseIdleConnections
}

// phoneInfo asks the authenticated GET /diagnostic/info for the phone's protocol and
// requires exactly phoneProtocol. An app that predates it answers with another status.
func phoneInfo(ctx context.Context, addr, token string) error {
	if e := loopbackAddr(addr); e != nil {
		return e
	}
	client, closeIdle := phoneHTTPClient()
	defer closeIdle()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/diagnostic/info", nil)
	if e != nil {
		return e
	}
	req.Header.Set("Proxy-Authorization", "Bearer "+token)
	resp, e := client.Do(req)
	if e != nil {
		return stageErr(stageInfo, 0, e)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusProxyAuthRequired:
		return stageErr(stageInfo, resp.StatusCode, fmt.Errorf("phone diagnostic HTTP %d (check foreground app and current token)", resp.StatusCode))
	case resp.StatusCode != http.StatusOK:
		return stageErr(stageInfo, resp.StatusCode, fmt.Errorf("the phone app did not answer /diagnostic/info (HTTP %d); it predates protocol %d, update the iOS app", resp.StatusCode, phoneProtocol))
	}
	var info struct {
		Protocol int `json:"protocol"`
	}
	if e := json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&info); e != nil {
		return stageErr(stageInfo, resp.StatusCode, errors.New("unreadable /diagnostic/info answer"))
	}
	if info.Protocol != phoneProtocol {
		return stageErr(stageInfo, resp.StatusCode, fmt.Errorf("phone speaks protocol %d, this build needs %d: update the iOS app and the desktop tool together", info.Protocol, phoneProtocol))
	}
	return nil
}

// probeStartup is the token-bearing part of the startup preflight, in this order: protocol
// 2 info, then the exact-byte USB application loop. Both are authenticated. A 407 at either
// step is errPhoneRejected, so a stale token is retried by the prompt and nothing else is.
func probeStartup(ctx context.Context, addr, token string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := phoneInfo(ctx, addr, token); err != nil {
		return asRejection(err)
	}
	_, err := checkPhone(ctx, addr, token, probeBytes)
	return asRejection(err)
}

// defaultProbeHost is a Steam content host on the default allowlist; whether it serves
// TLS at this name is confirmed on a device (H3). Keep it equal to steamaccel's default.
const defaultProbeHost = "steampipe.akamaized.net"

// defaultAllowedHosts mirrors the relay's default allowlist (steamaccel/config.go).
var defaultAllowedHosts = []string{"steamcontent.com", "steampipe.akamaized.net"}

// probeRoots is nil in production, which means the system root pool. Tests install a pool.
var probeRoots *x509.CertPool

// checkProbeHost applies the relay's allowlist rules to the probe host: a lowercase DNS
// name, never an IP or a host with a port, equal to or under an allowed suffix.
func checkProbeHost(host string, allowed []string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(host))
	labels := strings.Split(h, ".")
	ok := len(h) <= 253 && len(labels) >= 2 && net.ParseIP(h) == nil
	for _, l := range labels {
		ok = ok && len(l) >= 1 && len(l) <= 63 && l[0] != '-' && l[len(l)-1] != '-'
		for _, b := range []byte(l) {
			ok = ok && (b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-')
		}
	}
	if !ok {
		return "", errors.New("--probe-host must be a plain DNS name without port, path or IP")
	}
	if allowed == nil {
		allowed = defaultAllowedHosts
	}
	for _, a := range allowed {
		a = strings.ToLower(a)
		if h == a || strings.HasSuffix(h, "."+a) {
			return h, nil
		}
	}
	return "", errors.New("--probe-host is not on the allowlist (see --allow-hosts)")
}

type probeConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *probeConn) Read(p []byte) (int, error) { return c.r.Read(p) }

const cellularProbeTimeout = 20 * time.Second

// cellularProbe asks the phone for an authenticated CONNECT <host>:443 and then runs a TLS
// handshake across it, verified against the system roots (probeRoots in tests).
//
// A CONNECT 200 means the phone opened a cellular-scoped TCP connection to a peer its own
// public-address policy accepted; the desktop never resolves the name. That is the err
// result when it fails. A TLS failure is only tlsWarn: the host may simply not serve TLS
// under that name. Neither says anything about speed, and no rate is ever computed here.
func cellularProbe(ctx context.Context, addr, token, host string) (tlsWarn, err error) {
	if e := loopbackAddr(addr); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, cellularProbeTimeout)
	defer cancel()
	conn, e := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if e != nil {
		return nil, stageErr(stageConnect, 0, e)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	target := net.JoinHostPort(host, "443")
	if _, e := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\nProxy-Authorization: Bearer "+token+"\r\n\r\n"); e != nil {
		return nil, stageErr(stageConnect, 0, e)
	}
	reader := bufio.NewReaderSize(conn, 4096)
	var header []byte
	for { // ReadSlice bounds each read even if the peer never sends CRLF.
		fragment, readErr := reader.ReadSlice('\n')
		if len(header)+len(fragment) > 8192 {
			return nil, stageErr(stageConnect, 0, errors.New("CONNECT response headers too large"))
		}
		header = append(header, fragment...)
		if readErr != nil && readErr != bufio.ErrBufferFull {
			return nil, stageErr(stageConnect, 0, fmt.Errorf("CONNECT response: %w", readErr))
		}
		if readErr == nil && bytes.HasSuffix(header, []byte("\r\n\r\n")) {
			break
		}
	}
	resp, e := http.ReadResponse(bufio.NewReader(bytes.NewReader(header)), &http.Request{Method: http.MethodConnect})
	if e != nil {
		return nil, stageErr(stageConnect, 0, errors.New("invalid CONNECT response"))
	}
	switch {
	case resp.StatusCode == http.StatusProxyAuthRequired:
		return nil, stageErr(stageConnect, resp.StatusCode, fmt.Errorf("phone diagnostic HTTP %d (check foreground app and current token)", resp.StatusCode))
	case resp.StatusCode == http.StatusForbidden:
		return nil, stageErr(stageConnect, resp.StatusCode, errors.New("the phone refused the probe destination (its public-peer check failed)"))
	case resp.ProtoMajor != 1 || resp.StatusCode != http.StatusOK:
		return nil, stageErr(stageConnect, resp.StatusCode, fmt.Errorf("CONNECT rejected with status %d", resp.StatusCode))
	}
	tlsConn := tls.Client(&probeConn{Conn: conn, r: reader}, &tls.Config{ServerName: host, RootCAs: probeRoots, MinVersion: tls.VersionTLS12})
	if e := tlsConn.HandshakeContext(ctx); e != nil {
		return stageErr(stageTLS, 0, e), nil
	}
	return nil, nil
}

// Phone diagnostics exercise the authenticated USB application path, NOT cellular.
func checkPhone(ctx context.Context, addr, token string, count int) (float64, error) {
	if e := loopbackAddr(addr); e != nil {
		return 0, e
	}
	if count < 1 || count > 256*1024*1024 {
		return 0, errors.New("invalid diagnostic size")
	}
	client, closeIdle := phoneHTTPClient()
	defer closeIdle()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s/diagnostic/download?bytes=%d", addr, count), nil)
	if e != nil {
		return 0, e
	}
	req.Header.Set("Proxy-Authorization", "Bearer "+token)
	start := time.Now()
	resp, e := client.Do(req)
	if e != nil {
		return 0, stageErr(stageUSBLoop, 0, e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, stageErr(stageUSBLoop, resp.StatusCode, fmt.Errorf("phone diagnostic HTTP %d (check foreground app and current token)", resp.StatusCode))
	}
	if resp.ContentLength != int64(count) {
		return 0, errors.New("unexpected diagnostic content length")
	}
	buf := make([]byte, 64*1024)
	offset := 0
	for {
		n, err := resp.Body.Read(buf)
		for i, b := range buf[:n] {
			if b != byte((offset+i)%251) {
				return 0, errors.New("phone diagnostic payload corrupted")
			}
		}
		offset += n
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if offset > count {
			return 0, errors.New("oversized phone diagnostic")
		}
	}
	if offset != count {
		return 0, errors.New("truncated phone diagnostic")
	}
	return float64(count) * 8 / time.Since(start).Seconds() / 1e6, nil
}
