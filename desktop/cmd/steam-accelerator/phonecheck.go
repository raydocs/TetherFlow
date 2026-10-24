package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Phone diagnostics exercise the authenticated USB application path, NOT cellular.
func checkPhone(ctx context.Context, addr, token string, count int) (float64, error) {
	host, port, e := net.SplitHostPort(addr)
	if e != nil {
		return 0, e
	}
	ip := net.ParseIP(host)
	p, e := strconv.Atoi(port)
	if e != nil || p < 1 || p > 65535 || ip == nil || !ip.IsLoopback() {
		return 0, errors.New("phone endpoint must be numeric loopback")
	}
	if count < 1 || count > 256*1024*1024 {
		return 0, errors.New("invalid diagnostic size")
	}
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 * 1024}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("unexpected phone redirect") }}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s/diagnostic/download?bytes=%d", addr, count), nil)
	if e != nil {
		return 0, e
	}
	req.Header.Set("Proxy-Authorization", "Bearer "+token)
	start := time.Now()
	resp, e := client.Do(req)
	if e != nil {
		return 0, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("phone diagnostic HTTP %d (check foreground app and current token)", resp.StatusCode)
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
