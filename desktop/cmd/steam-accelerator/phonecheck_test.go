package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPhoneCheck(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		ok     bool
	}{{"valid", 200, string([]byte{0, 1, 2}), true}, {"corrupt", 200, "bad", false}, {"auth", 407, "", false}} {
		t.Run(test.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "Bearer private-token" {
					t.Error("auth missing")
				}
				if r.URL.RawQuery != "bytes=3" {
					t.Error(r.URL)
				}
				w.Header().Set("Content-Length", "3")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer s.Close()
			rate, e := checkPhone(context.Background(), strings.TrimPrefix(s.URL, "http://"), "private-token", 3)
			if (e == nil) != test.ok {
				t.Fatal(e)
			}
			if test.ok && rate <= 0 {
				t.Fatal(rate)
			}
		})
	}
}
func TestPhoneCheckRejectsRemote(t *testing.T) {
	if _, e := checkPhone(context.Background(), "8.8.8.8:80", "token", 3); e == nil {
		t.Fatal("remote accepted")
	}
}
