package main

import "testing"

func TestValidateHomeRejectsUnassigned(t *testing.T) {
	for _, s := range []string{"", "127.0.0.1", "8.8.8.8", "::1", "not-ip", "192.168.254.253"} {
		if validateHome(s) == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
