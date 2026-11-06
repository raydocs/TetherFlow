package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testLease = "0123456789abcdef0123456789abcdef"
const otherLease = "fedcba9876543210fedcba9876543210"

// tempHosts writes a temporary hosts file; tests never touch the system hosts file.
func tempHosts(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
func block(lease, ip string) string {
	return "# BEGIN TetherFlow Steam " + lease + "\n" + ip + " lancache.steamcontent.com\n# END TetherFlow Steam " + lease + "\n"
}
func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// listenProof stands in for port 80 on the home address.
func listenProof(t *testing.T) (net.Listener, string) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("loopback listener unavailable")
	}
	t.Cleanup(func() { l.Close() })
	old := reclaimProofPort
	reclaimProofPort = strings.Split(l.Addr().String(), ":")[1]
	t.Cleanup(func() { reclaimProofPort = old })
	return l, "127.0.0.1"
}

func TestReclaimStaleOnlyWhenIPMatchesHome(t *testing.T) {
	l, home := listenProof(t)
	foreign := "127.0.0.1 localhost\n"
	t.Run("matching ip with held port is removed and foreign bytes survive", func(t *testing.T) {
		p := tempHosts(t, foreign+block(testLease, home)+"10.0.0.9 later.example\n")
		if err := reclaimStale(p, home, l); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, p); got != foreign+"10.0.0.9 later.example\n" {
			t.Fatalf("hosts = %q", got)
		}
	})
	t.Run("different ip is refused with the exact command", func(t *testing.T) {
		content := foreign + block(testLease, "192.168.50.7")
		p := tempHosts(t, content)
		err := reclaimStale(p, home, l)
		if err == nil || !strings.Contains(err.Error(), "--recover-lease "+testLease) {
			t.Fatalf("err = %v", err)
		}
		if readFile(t, p) != content {
			t.Fatal("refused reclaim changed hosts")
		}
	})
	t.Run("matching ip without a held listener is refused", func(t *testing.T) {
		content := foreign + block(testLease, home)
		p := tempHosts(t, content)
		if err := reclaimStale(p, home, nil); err == nil || !strings.Contains(err.Error(), "--recover-lease "+testLease) {
			t.Fatalf("err = %v", err)
		}
		if readFile(t, p) != content {
			t.Fatal("reclaim ran without proof of death")
		}
	})
	t.Run("listener on another port is no proof", func(t *testing.T) {
		old := reclaimProofPort
		reclaimProofPort = "80"
		defer func() { reclaimProofPort = old }()
		content := block(testLease, home)
		p := tempHosts(t, content)
		if err := reclaimStale(p, home, l); err == nil {
			t.Fatal("accepted a listener that is not on port 80")
		}
		if readFile(t, p) != content {
			t.Fatal("changed hosts")
		}
	})
	t.Run("listener on another address is no proof", func(t *testing.T) {
		content := block(testLease, "192.168.50.7")
		p := tempHosts(t, content)
		if err := reclaimStale(p, "192.168.50.7", l); err == nil {
			t.Fatal("accepted a listener bound to a different address")
		}
		if readFile(t, p) != content {
			t.Fatal("changed hosts")
		}
	})
	t.Run("clean hosts is a no-op", func(t *testing.T) {
		p := tempHosts(t, foreign)
		if err := reclaimStale(p, home, l); err != nil || readFile(t, p) != foreign {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("one matching and one foreign-ip lease reclaims neither", func(t *testing.T) {
		content := block(testLease, home) + block(otherLease, "192.168.50.7")
		p := tempHosts(t, content)
		if err := reclaimStale(p, home, l); err == nil {
			t.Fatal("expected refusal")
		}
		if readFile(t, p) != content {
			t.Fatal("partial reclaim")
		}
	})
}

func TestReclaimRefusesMalformedOrForeignBlock(t *testing.T) {
	l, home := listenProof(t)
	cases := map[string]string{
		"foreign record hidden in our block": "# BEGIN TetherFlow Steam " + testLease + "\n" + home + " lancache.steamcontent.com\n6.6.6.6 evil.example\n# END TetherFlow Steam " + testLease + "\n",
		"wrong host in our block":            "# BEGIN TetherFlow Steam " + testLease + "\n" + home + " other.example\n# END TetherFlow Steam " + testLease + "\n",
		"unterminated block":                 "# BEGIN TetherFlow Steam " + testLease + "\n" + home + " lancache.steamcontent.com\n",
		"invalid lease":                      "# BEGIN TetherFlow Steam notalease\n" + home + " lancache.steamcontent.com\n# END TetherFlow Steam notalease\n",
		"foreign override of the domain":     home + " lancache.steamcontent.com\n",
		"duplicate lease":                    block(testLease, home) + block(testLease, home),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := tempHosts(t, "127.0.0.1 localhost\n"+content)
			before := readFile(t, p)
			if err := reclaimStale(p, home, l); err == nil {
				t.Fatal("reclaim accepted it")
			}
			if readFile(t, p) != before {
				t.Fatal("hosts changed")
			}
		})
	}
}

func TestRecoveryLogHasLeaseAndNoToken(t *testing.T) {
	token := strings.Repeat("T", 40)
	logPath := filepath.Join(t.TempDir(), "recovery.log")
	// A refusal (modified block) is permanent; the error text must reach the log redacted.
	p := tempHosts(t, "# note "+token+"\n# BEGIN TetherFlow Steam "+testLease+"\n1.1.1.1 other.example\n# END TetherFlow Steam "+testLease+"\n")
	err := cleanupHosts(context.Background(), p, testLease, time.Second, func() error { return nil }, logPath, token)
	if err == nil {
		t.Fatal("expected refusal")
	}
	data := readFile(t, logPath)
	if !strings.Contains(data, "lease="+testLease) || !strings.Contains(data, "--recover-lease "+testLease) {
		t.Fatalf("log lacks lease: %q", data)
	}
	// Direct redaction: an error that carries the token must not leak it.
	writeRecoveryLog(logPath, testLease, "test", errors.New("dial with token "+token), token)
	data = readFile(t, logPath)
	if strings.Contains(data, token) {
		t.Fatalf("token in recovery log: %q", data)
	}
	if !strings.Contains(data, "[redacted]") {
		t.Fatalf("redaction marker missing: %q", data)
	}
	// Newlines in an error cannot forge a second log line.
	writeRecoveryLog(logPath, testLease, "test", errors.New("a\nlease=forged"), token)
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, logPath)), "\n") {
		if !strings.Contains(line, "lease="+testLease) {
			t.Fatalf("unattributed line %q", line)
		}
	}
	if runtimeSupportsModes() {
		st, err := os.Stat(logPath)
		if err != nil || st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("recovery log mode %v %v", st, err)
		}
	}
}

func TestDefaultRecoveryLogIsNextToReport(t *testing.T) {
	dir := t.TempDir()
	got := defaultRecoveryLog(filepath.Join(dir, "status.json"))
	if filepath.Dir(got) != dir || !filepath.IsAbs(got) {
		t.Fatalf("got %q", got)
	}
}

func TestFlushRetriesThreeAttempts(t *testing.T) {
	old := flushRetryDelay
	flushRetryDelay = time.Millisecond
	defer func() { flushRetryDelay = old }()
	p := tempHosts(t, block(testLease, "192.168.1.5"))
	var calls int32
	flush := func() error {
		if atomic.AddInt32(&calls, 1) < 3 {
			return errors.New("flush busy")
		}
		return nil
	}
	if err := cleanupHosts(context.Background(), p, testLease, time.Second, flush, ""); err != nil {
		t.Fatalf("flush should succeed on the third attempt: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
	if readFile(t, p) != "" {
		t.Fatal("block not removed")
	}
	// A permanent flush failure stops at three attempts and is logged.
	p = tempHosts(t, block(testLease, "192.168.1.5"))
	logPath := filepath.Join(t.TempDir(), "r.log")
	calls = 0
	err := cleanupHosts(context.Background(), p, testLease, time.Second, func() error { atomic.AddInt32(&calls, 1); return errors.New("down") }, logPath)
	if err == nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if !strings.Contains(readFile(t, logPath), "lease="+testLease) {
		t.Fatal("flush failure not logged")
	}
}

func TestCleanupNoFlushWhenUnchanged(t *testing.T) {
	p := tempHosts(t, "127.0.0.1 localhost\n")
	if err := cleanupHosts(context.Background(), p, testLease, time.Second, func() error { return errors.New("must not flush") }, ""); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupPermanentRefusalIsNotRetried(t *testing.T) {
	content := "# BEGIN TetherFlow Steam " + testLease + "\n1.2.3.4 evil.example\n# END TetherFlow Steam " + testLease + "\n"
	p := tempHosts(t, content)
	logPath := filepath.Join(t.TempDir(), "r.log")
	start := time.Now()
	err := cleanupHosts(context.Background(), p, testLease, time.Minute, func() error { return nil }, logPath)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	if readFile(t, p) != content {
		t.Fatal("refused cleanup changed hosts")
	}
	if !strings.Contains(readFile(t, logPath), testLease) {
		t.Fatal("refusal not logged")
	}
}
