package steamaccel

import (
	"bufio"
	"net"
	"os"
	"strings"
	"testing"
)

type ipVector struct {
	line    int
	address string
	go_     bool
	swift   bool
}

func loadIPVectors(t *testing.T) []ipVector {
	t.Helper()
	f, err := os.Open("../../../integration/public-ip-vectors.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decision := func(line int, s string) bool {
		switch s {
		case "allow":
			return true
		case "deny":
			return false
		}
		t.Fatalf("line %d: decision %q is neither allow nor deny", line, s)
		return false
	}
	var out []ipVector
	header := false
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		text := scanner.Text()
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		cols := strings.Split(text, "\t")
		if len(cols) != 3 {
			t.Fatalf("line %d: want 3 tab-separated columns, got %d", n, len(cols))
		}
		if !header {
			if strings.Join(cols, ",") != "address,go,swift" {
				t.Fatalf("line %d: unexpected header %q", n, text)
			}
			header = true
			continue
		}
		out = append(out, ipVector{n, cols[0], decision(n, cols[1]), decision(n, cols[2])})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !header || len(out) < 100 {
		t.Fatalf("vector file too small or missing header: %d rows", len(out))
	}
	return out
}

// TestPublicIPVectors pins publicIP to the vector file shared with the Swift PublicAddress policy.
func TestPublicIPVectors(t *testing.T) {
	var allowed, denied int
	for _, v := range loadIPVectors(t) {
		if got := publicIP(net.ParseIP(v.address)); got != v.go_ {
			t.Errorf("line %d: publicIP(%s) = %v, want %v", v.line, v.address, got, v.go_)
		}
		if v.go_ {
			allowed++
		} else {
			denied++
		}
	}
	if allowed < 20 || denied < 60 {
		t.Fatalf("vector coverage too thin: %d allow, %d deny", allowed, denied)
	}
}

// The only intended Go/Swift difference is the well-known NAT64 prefix with an embedded public IPv4.
func TestPublicIPVectorsOnlyDocumentedDivergence(t *testing.T) {
	nat64 := net.IPNet{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)}
	var diverging int
	for _, v := range loadIPVectors(t) {
		if v.go_ == v.swift {
			continue
		}
		diverging++
		ip := net.ParseIP(v.address)
		if ip == nil || !nat64.Contains(ip) || v.go_ || !v.swift {
			t.Errorf("line %d: %s diverges outside the documented NAT64 exception (go=%v swift=%v)",
				v.line, v.address, v.go_, v.swift)
		}
	}
	if diverging == 0 {
		t.Fatal("expected the documented NAT64 exception rows")
	}
}
