package steamsetup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeHosts(t *testing.T, content string) (dir, p string) {
	t.Helper()
	dir = t.TempDir()
	p = filepath.Join(dir, "hosts")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return
}

func plantJournal(t *testing.T, p, l string, old, next string) string {
	t.Helper()
	canon, err := canonicalHostsPath(p)
	if err != nil {
		t.Fatal(err)
	}
	jp := journalPath(canon, l)
	if err := os.WriteFile(jp, encodeJournal(l, []byte(old), []byte(next)), 0600); err != nil {
		t.Fatal(err)
	}
	return jp
}

func mustRewrite(t *testing.T, base, ip string) string {
	t.Helper()
	b, err := Rewrite([]byte(base), ip, lease)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRecoveryPreservesForeignAppendAfterApply(t *testing.T) {
	installed := mustRewrite(t, crashBase, "192.168.1.5")
	_, p := writeHosts(t, installed+"10.0.0.9 foreign.local\n")
	jp := plantJournal(t, p, lease, crashBase, installed)
	if _, err := Update(p, "", lease); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != crashBase+"10.0.0.9 foreign.local\n" {
		t.Fatalf("%q", got)
	}
	if _, err := os.Lstat(jp); !os.IsNotExist(err) {
		t.Fatal("journal kept", err)
	}
}

func TestRecoveryRefusesForeignEditOfTornFile(t *testing.T) {
	installed := mustRewrite(t, crashBase, "192.168.1.5")
	torn := installed[:len(installed)/2] + "# foreign edit\n"
	_, p := writeHosts(t, torn)
	jp := plantJournal(t, p, lease, crashBase, installed)
	_, err := Update(p, "", lease)
	if !errors.Is(err, ErrNeedsManualRecovery) || !strings.Contains(err.Error(), jp) {
		t.Fatalf("want manual recovery naming %s, got %v", jp, err)
	}
	if IsTransient(err) {
		t.Fatal("manual recovery must not be transient")
	}
	got, _ := os.ReadFile(p)
	if string(got) != torn {
		t.Fatalf("bytes changed: %q", got)
	}
	if _, err := os.Lstat(jp); err != nil {
		t.Fatal("journal removed", err)
	}
}

func TestUnresolvedJournalBlocksLaterUpdate(t *testing.T) {
	_, p := writeHosts(t, "# edited by someone\n")
	jp := plantJournal(t, p, lease, crashBase, mustRewrite(t, crashBase, "192.168.1.5"))
	other := strings.Repeat("b", 32)
	for i := 0; i < 2; i++ {
		for _, l := range []string{lease, other} {
			if _, err := Update(p, "192.168.1.6", l); !errors.Is(err, ErrNeedsManualRecovery) {
				t.Fatalf("update %s: %v", l, err)
			}
		}
	}
	if got, _ := os.ReadFile(p); string(got) != "# edited by someone\n" {
		t.Fatalf("%q", got)
	}
	if _, err := os.Lstat(jp); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptOrLinkedJournalIsNotFollowed(t *testing.T) {
	dir, p := writeHosts(t, crashBase)
	canon, _ := canonicalHostsPath(p)
	jp := journalPath(canon, lease)
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("do not delete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, jp); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := Update(p, "192.168.1.5", lease); !errors.Is(err, ErrNeedsManualRecovery) {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(secret); string(got) != "do not delete" {
		t.Fatal("link target modified")
	}
	_ = os.Remove(jp)
	if err := os.WriteFile(jp, bytes.Repeat([]byte("x"), 100), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(p, "192.168.1.5", lease); !errors.Is(err, ErrNeedsManualRecovery) {
		t.Fatal(err)
	}
}

func TestTornJournalTempIgnored(t *testing.T) {
	dir, p := writeHosts(t, crashBase)
	canon, _ := canonicalHostsPath(p)
	tmp := journalPath(canon, lease) + tmpSuffix
	full := encodeJournal(lease, []byte(crashBase), []byte("whatever"))
	if err := os.WriteFile(tmp, full[:len(full)/2], 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := Update(p, "192.168.1.5", lease)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if got, _ := os.ReadFile(p); string(got) != mustRewrite(t, crashBase, "192.168.1.5") {
		t.Fatalf("%q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatal("temp not removed", entries)
	}
}

func TestJournalRemovedOnSuccess(t *testing.T) {
	dir, p := writeHosts(t, crashBase)
	for _, ip := range []string{"192.168.1.5", ""} {
		if _, err := Update(p, ip, lease); err != nil {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Fatal(entries)
		}
	}
}

func TestRecoveryClassifiesTornPrefixOldSuffix(t *testing.T) {
	old, next := "AAAAAAAAAA", "bbbbbbbbbbbbbbb"
	for k := 0; k <= len(next); k++ {
		cur := next[:k]
		if k < len(old) {
			cur += old[k:]
		}
		st := classify([]byte(cur), []byte(old), []byte(next))
		if k == 0 && st != stateOld || k == len(next) && st != stateNew || k > 0 && k < len(next) && st != stateTorn {
			t.Fatalf("k=%d cur=%q state=%v", k, cur, st)
		}
	}
	// Shrinking update, killed after the full write but before the truncate.
	if st := classify([]byte("bbbbbbAAAA"), []byte("AAAAAAAAAA"), []byte("bbbbbb")); st != stateTorn {
		t.Fatal(st)
	}
	for _, cur := range []string{"bbbbxxAAAA", "AAAAAAAAAAy", "zzz", "bbbbbbAAAAy"} {
		if st := classify([]byte(cur), []byte("AAAAAAAAAA"), []byte("bbbbbb")); st == stateTorn || st == stateOld || st == stateNew {
			t.Fatalf("%q classified %v", cur, st)
		}
	}
	if st := classify([]byte("bbbbbbFOREIGN"), []byte("AAAAAAAAAA"), []byte("bbbbbb")); st != stateNewForeign {
		t.Fatal(st)
	}
}

func TestRefusalIsNeverRetried(t *testing.T) {
	_, p := writeHosts(t, "1.2.3.4 lancache.steamcontent.com\n")
	attempts := 0
	retryObserve = func(int) { attempts++ }
	defer func() { retryObserve = nil }()
	_, err := UpdateWithRetry(context.Background(), p, "192.168.1.5", lease, RetryPolicy{Initial: time.Millisecond, Max: 2 * time.Millisecond, Total: 2 * time.Second})
	if err == nil || attempts != 1 || IsTransient(err) {
		t.Fatal(attempts, err)
	}
}

func TestRetryHonoursTotalBudget(t *testing.T) {
	_, p := writeHosts(t, crashBase)
	holder, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := lockFile(holder); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	retryObserve = func(int) { attempts++ }
	defer func() { retryObserve = nil }()
	start := time.Now()
	_, err = UpdateWithRetry(context.Background(), p, "192.168.1.5", lease, RetryPolicy{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond, Total: 200 * time.Millisecond})
	took := time.Since(start)
	if err == nil || !IsTransient(err) {
		t.Fatal("want transient failure after budget, got", err)
	}
	if attempts < 3 || took < 200*time.Millisecond || took > 2*time.Second {
		t.Fatalf("attempts=%d took=%v", attempts, took)
	}
	attempts = 0
	if _, err = UpdateWithRetry(context.Background(), p, "192.168.1.5", lease, RetryPolicy{}); err == nil || attempts != 1 {
		t.Fatal("zero Total must be a single attempt", attempts, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = UpdateWithRetry(ctx, p, "192.168.1.5", lease, RetryPolicy{Initial: time.Hour, Total: time.Hour}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// A real second process holds the hosts lock, then releases it while UpdateWithRetry waits.
func TestTransientLockThenSuccess(t *testing.T) {
	_, p := writeHosts(t, crashBase)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHolderHelper$")
	cmd.Env = append(os.Environ(), "TF_LOCK_PATH="+p)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	buf := make([]byte, len("locked\n"))
	if _, err := out.Read(buf); err != nil || string(buf) != "locked\n" {
		t.Fatalf("holder: %q %v", buf, err)
	}
	attempts := 0
	retryObserve = func(int) { attempts++ }
	defer func() { retryObserve = nil }()
	changed, err := UpdateWithRetry(context.Background(), p, "192.168.1.5", lease, RetryPolicy{Initial: 10 * time.Millisecond, Max: 50 * time.Millisecond, Total: 10 * time.Second})
	if err != nil || !changed || attempts < 2 {
		t.Fatal(changed, attempts, err)
	}
}

func TestLockHolderHelper(t *testing.T) {
	p := os.Getenv("TF_LOCK_PATH")
	if p == "" {
		t.Skip("helper process only")
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil || lockFile(f) != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString("locked\n")
	time.Sleep(400 * time.Millisecond)
	unlockFile(f)
	os.Exit(0)
}

func TestInspectReportsLeasesOverridesAndJournalsWithoutWriting(t *testing.T) {
	l2 := strings.Repeat("c", 32)
	data := mustRewrite(t, "# a\n", "192.168.1.5")
	data += begin + l2 + "\nbad line here\n" + end + l2 + "\n"
	data += "1.2.3.4 LANCACHE.steamcontent.com. # other tool\n"
	dir, p := writeHosts(t, data)
	jp := plantJournal(t, p, lease, "x", "y")
	canon, _ := canonicalHostsPath(p)
	_ = os.WriteFile(journalPath(canon, l2)+tmpSuffix, []byte("torn"), 0600)
	before, _ := os.ReadDir(dir)
	in, err := Inspect(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Leases) != 1 || in.Leases[0] != (LeaseEntry{Lease: lease, IP: "192.168.1.5"}) || !in.ForeignOverride || !in.Malformed {
		t.Fatalf("%+v", in)
	}
	if len(in.PendingJournals) != 1 || in.PendingJournals[0] != jp {
		t.Fatalf("%+v", in.PendingJournals)
	}
	after, _ := os.ReadDir(dir)
	if len(before) != len(after) {
		t.Fatal("inspect changed the directory")
	}
	if got, _ := os.ReadFile(p); string(got) != data {
		t.Fatal("inspect changed hosts")
	}
}

func TestUpdateKeepsFileIdentity(t *testing.T) {
	_, p := writeHosts(t, crashBase)
	before, _ := os.Stat(p)
	for _, ip := range []string{"192.168.1.5", ""} {
		if _, err := Update(p, ip, lease); err != nil {
			t.Fatal(err)
		}
		after, _ := os.Stat(p)
		if !os.SameFile(before, after) {
			t.Fatal("file identity changed")
		}
	}
}
