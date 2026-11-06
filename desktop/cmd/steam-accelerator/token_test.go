package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tetherflow-desktop/internal/steamaccel"
)

const (
	tokenA       = "0123456789abcdefghjkmnpqrstvwxyz"
	tokenB       = "zyxwvtsrqpnmkjhgfedcba9876543210"
	tokenBGroups = "ZYXW-vtsr-qpnm-kjhg-fedc-ba98-7654-3210"
)

func lineReader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

func TestReadTokenLineBoundsAndTrim(t *testing.T) {
	r := lineReader("  " + tokenA + " \r\n\n" + strings.Repeat("a", maxTokenLine) + "\n" +
		strings.Repeat("b", maxTokenLine+1) + "\n" + strings.Repeat("c", 1<<20) + "\r\n" + "last-no-newline")
	want := []struct {
		line string
		err  error
	}{
		{tokenA, nil},
		{"", nil},
		{strings.Repeat("a", maxTokenLine), nil},
		{"", errTokenLineTooLong},
		{"", errTokenLineTooLong},
		{"last-no-newline", nil},
		{"", io.EOF},
	}
	for i, w := range want {
		got, err := readTokenLine(r)
		if got != w.line || !errors.Is(err, w.err) && err != w.err {
			t.Fatalf("read %d: got (%d bytes, %v), want (%d bytes, %v)", i, len(got), err, len(w.line), w.err)
		}
	}
	if _, err := readTokenLine(lineReader(strings.Repeat("x", 9000))); !errors.Is(err, errTokenLineTooLong) {
		t.Fatalf("overlong final line without newline: %v", err)
	}
	if msg := errTokenLineTooLong.Error(); strings.Contains(msg, "bbbb") {
		t.Fatal("error echoes input")
	}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}
func (l *logSink) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
func (l *logSink) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

func newTestRelay(t *testing.T, token string) *steamaccel.Relay {
	t.Helper()
	r, err := steamaccel.New(steamaccel.Config{HomeIPv4: "127.0.0.1", PhoneToken: token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// scriptedInput feeds read() from a list; after the list it reports EOF.
func scriptedInput(lines ...string) func() (string, error) {
	var mu sync.Mutex
	i := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(lines) {
			return "", io.EOF
		}
		i++
		if lines[i-1] == "<toolong>" {
			return "", errTokenLineTooLong
		}
		return lines[i-1], nil
	}
}

// staleRelay drives the refresher against a real Relay: status comes from the
// relay's generation, and Stale is controlled by the test (a real 407 is covered
// in internal/steamaccel).
type staleRelay struct {
	relay *steamaccel.Relay
	stale atomic.Bool
}

func (s *staleRelay) status() steamaccel.PhoneAuthStatus {
	st := s.relay.Snapshot().PhoneAuth
	st.Stale = s.stale.Load()
	return st
}
func (s *staleRelay) set(token string) error {
	if err := s.relay.SetPhoneToken(token); err != nil {
		return err
	}
	s.stale.Store(false)
	return nil
}

// strList is a goroutine-safe string list shared between a test and the refresher.
type strList struct {
	mu sync.Mutex
	v  []string
}

func (l *strList) add(s string) { l.mu.Lock(); l.v = append(l.v, s); l.mu.Unlock() }
func (l *strList) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.v...)
}

func newRefresher(sr *staleRelay, sink *logSink, read func() (string, error), probe func(context.Context, string) error, remembered *strList) *refresher {
	return &refresher{status: sr.status, read: read, probe: probe, set: sr.set,
		remember: remembered.add, logf: sink.logf, poll: time.Millisecond}
}

func runRefresher(t *testing.T, rf *refresher) (cancel func(), done <-chan struct{}) {
	ctx, c := context.WithCancel(context.Background())
	d := make(chan struct{})
	go func() { rf.run(ctx); close(d) }()
	t.Cleanup(c)
	return c, d
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRefreshSwapsOnlyAfterProbeSuccess(t *testing.T) {
	sr := &staleRelay{relay: newTestRelay(t, tokenA)}
	sink := &logSink{}
	var probed strList
	var remembered strList
	probe := func(_ context.Context, tok string) error {
		probed.add(tok)
		if got := sr.relay.Snapshot().PhoneAuth.Generation; got != 1 {
			t.Errorf("token swapped before the probe finished (generation %d)", got)
		}
		return nil
	}
	rf := newRefresher(sr, sink, scriptedInput(tokenBGroups), probe, &remembered)
	sr.stale.Store(true)
	_, done := runRefresher(t, rf)
	waitFor(t, "swap", func() bool { return len(remembered.all()) == 1 })
	if p := probed.all(); len(p) != 1 || p[0] != tokenB {
		t.Fatalf("probe received %v, want the normalized candidate", p)
	}
	if r := remembered.all(); sr.stale.Load() || r[0] != tokenB || sr.relay.Snapshot().PhoneAuth.Generation != 2 {
		t.Fatalf("stale=%v remembered=%v", sr.stale.Load(), r)
	}
	_ = done
}

func TestRefreshKeepsOldTokenOnProbeFailure(t *testing.T) {
	sr := &staleRelay{relay: newTestRelay(t, tokenA)}
	sink := &logSink{}
	var remembered strList
	var calls atomic.Int32
	probe := func(_ context.Context, tok string) error {
		switch calls.Add(1) {
		case 1:
			return errPhoneRejected
		case 2:
			return errors.New("dial tcp 127.0.0.1:8283: connect: refused " + tok)
		}
		return nil
	}
	// invalid format, too long, rejected, unreachable, then good.
	rf := newRefresher(sr, sink, scriptedInput("short", "<toolong>", tokenB, tokenB, tokenB), probe, &remembered)
	sr.stale.Store(true)
	runRefresher(t, rf)
	waitFor(t, "good token", func() bool { return len(remembered.all()) == 1 })
	if n := calls.Load(); n != 3 {
		t.Fatalf("probe calls %d, want 3 (invalid and overlong input are never probed)", n)
	}
	if got := sink.count("the old token stays active"); got != 2 {
		t.Fatalf("old-token-kept messages %d, want 2:\n%s", got, sink.text())
	}
	if strings.Contains(sink.text(), tokenB) {
		t.Fatalf("log leaks the token:\n%s", sink.text())
	}
}

func TestRefreshProbeFailureNeverSwaps(t *testing.T) {
	sr := &staleRelay{relay: newTestRelay(t, tokenA)}
	sink := &logSink{}
	var remembered strList
	rf := newRefresher(sr, sink, scriptedInput(tokenB, tokenB), func(context.Context, string) error { return errPhoneRejected }, &remembered)
	sr.stale.Store(true)
	_, done := runRefresher(t, rf)
	<-done // EOF after the two attempts
	if g := sr.relay.Snapshot().PhoneAuth.Generation; g != 1 || !sr.stale.Load() || len(remembered.all()) != 0 {
		t.Fatalf("state changed after failed probes: generation=%d stale=%v remembered=%v", g, sr.stale.Load(), remembered.all())
	}
}

func TestRefreshEOFKeepsRunning(t *testing.T) {
	sr := &staleRelay{relay: newTestRelay(t, tokenA)}
	sink := &logSink{}
	var remembered strList
	rf := newRefresher(sr, sink, scriptedInput(), func(context.Context, string) error { t.Error("probe called"); return nil }, &remembered)
	sr.stale.Store(true)
	cancel, done := runRefresher(t, rf)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresher did not stop after EOF")
	}
	cancel()
	if sink.count("refresh disabled") != 1 || sink.count("PHONE TOKEN REJECTED") != 1 {
		t.Fatalf("expected one prompt and one disable line:\n%s", sink.text())
	}
	// The relay itself is untouched and still usable.
	if st := sr.relay.Snapshot(); st.PhoneAuth.Generation != 1 {
		t.Fatalf("generation %d", st.PhoneAuth.Generation)
	}
	if err := sr.relay.SetPhoneToken(tokenB); err != nil {
		t.Fatalf("relay unusable after refresh was disabled: %v", err)
	}
}

func TestRefreshPromptsOncePerGeneration(t *testing.T) {
	sr := &staleRelay{relay: newTestRelay(t, tokenA)}
	sink := &logSink{}
	var remembered strList
	rf := newRefresher(sr, sink, scriptedInput("bad", "bad", tokenB, "bad", tokenA), func(context.Context, string) error { return nil }, &remembered)
	sr.stale.Store(true)
	_, done := runRefresher(t, rf)
	waitFor(t, "generation 2", func() bool { return sr.relay.Snapshot().PhoneAuth.Generation == 2 })
	if got := sink.count("PHONE TOKEN REJECTED"); got != 1 {
		t.Fatalf("prompts for generation 1: %d (failed candidates must not repeat the prompt)\n%s", got, sink.text())
	}
	time.Sleep(20 * time.Millisecond) // still not stale: no new prompt
	if sink.count("PHONE TOKEN REJECTED") != 1 {
		t.Fatal("prompted without a stale state")
	}
	sr.stale.Store(true) // a 407 for generation 2
	waitFor(t, "generation 3", func() bool { return sr.relay.Snapshot().PhoneAuth.Generation == 3 })
	if got := sink.count("PHONE TOKEN REJECTED"); got != 2 {
		t.Fatalf("prompts after two generations: %d\n%s", got, sink.text())
	}
	if sink.count("(generation 1)") != 1 || sink.count("(generation 2)") != 1 {
		t.Fatalf("prompt did not name each generation once:\n%s", sink.text())
	}
	_ = done
}

func TestStartupRepromptLimit(t *testing.T) {
	t.Run("rejected tokens use three attempts", func(t *testing.T) {
		var reads, probes int
		sink := &logSink{}
		read := func() (string, error) { reads++; return tokenB, nil }
		_, err := acquireStartupToken(read, func(string) error { probes++; return errPhoneRejected }, sink.logf)
		if err == nil || reads != 3 || probes != 3 {
			t.Fatalf("err=%v reads=%d probes=%d", err, reads, probes)
		}
		if strings.Contains(err.Error()+sink.text(), tokenB) {
			t.Fatal("token in output")
		}
	})
	t.Run("invalid and overlong input also count", func(t *testing.T) {
		sink := &logSink{}
		_, err := acquireStartupToken(scriptedInput("x", "<toolong>", "y", tokenB), func(string) error { t.Error("probed invalid input"); return nil }, sink.logf)
		if err == nil {
			t.Fatal("fourth line must never be read")
		}
	})
	t.Run("accepted on the third attempt", func(t *testing.T) {
		n := 0
		tok, err := acquireStartupToken(scriptedInput("x", tokenA, tokenB), func(s string) error {
			if n++; n == 1 {
				return errPhoneRejected
			}
			return nil
		}, (&logSink{}).logf)
		if err != nil || tok != tokenB {
			t.Fatalf("tok=%q err=%v", tok, err)
		}
	})
	t.Run("phone absent is not a token problem", func(t *testing.T) {
		reads := 0
		_, err := acquireStartupToken(func() (string, error) { reads++; return tokenB, nil },
			func(string) error { return errors.New("connection refused") }, (&logSink{}).logf)
		if err == nil || reads != 1 || !strings.Contains(err.Error(), "preflight failed") {
			t.Fatalf("err=%v reads=%d", err, reads)
		}
	})
	t.Run("EOF ends startup", func(t *testing.T) {
		_, err := acquireStartupToken(scriptedInput(), func(string) error { return nil }, (&logSink{}).logf)
		if err == nil || !strings.Contains(err.Error(), "no iPhone session token on stdin") {
			t.Fatal(err)
		}
	})
}

func fakePhone(t *testing.T, good string, requests *atomic.Int32) string {
	t.Helper()
	pattern := make([]byte, 64*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Proxy-Authorization") != "Bearer "+good {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
		w.Header().Set("Content-Length", strconv.Itoa(n))
		off := 0
		for off < n {
			c := len(pattern)
			if n-off < c {
				c = n - off
			}
			for i := 0; i < c; i++ {
				pattern[i] = byte((off + i) % 251)
			}
			_, _ = w.Write(pattern[:c])
			off += c
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestProbeClassifiesRejection(t *testing.T) {
	var n atomic.Int32
	addr := fakePhone(t, tokenA, &n)
	if err := probePhone(context.Background(), addr, tokenA); err != nil {
		t.Fatalf("good token: %v", err)
	}
	if err := probePhone(context.Background(), addr, tokenB); !errors.Is(err, errPhoneRejected) {
		t.Fatalf("407 not classified as a rejection: %v", err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	if err := probePhone(context.Background(), deadAddr, tokenA); err == nil || errors.Is(err, errPhoneRejected) {
		t.Fatalf("unreachable phone must not look like a bad token: %v", err)
	}
}

func TestNoTokenInLogsOrReport(t *testing.T) {
	sink := &logSink{}
	secrets := []string{tokenA, tokenB, tokenBGroups, strings.ToUpper(tokenB)}
	leak := func(text string) string {
		lower := strings.NewReplacer("-", "", " ", "").Replace(strings.ToLower(text)) // also catches grouped forms
		for _, s := range secrets {
			plain := strings.ToLower(strings.ReplaceAll(s, "-", ""))
			for i := 0; i+12 <= len(plain); i += 4 { // any 12-character window of the token
				if w := plain[i : i+12]; strings.Contains(lower, w) {
					return w
				}
			}
		}
		return ""
	}
	check := func(what, text string) {
		if w := leak(text); w != "" {
			t.Fatalf("%s contains a token fragment %q:\n%s", what, w, text)
		}
	}
	if leak("x "+tokenBGroups) == "" || leak("harmless") != "" {
		t.Fatal("leak detector is broken")
	}
	// Errors that (wrongly) carry the token are redacted before they are logged or returned.
	_, err := acquireStartupToken(scriptedInput(tokenBGroups), func(tok string) error { return errors.New("boom " + tok) }, sink.logf)
	if err == nil {
		t.Fatal("expected error")
	}
	check("startup error", err.Error())
	sr := &staleRelay{relay: newTestRelay(t, tokenA)}
	var remembered strList
	var n atomic.Int32
	rf := newRefresher(sr, sink, scriptedInput(tokenBGroups, tokenB, "short"), func(_ context.Context, tok string) error {
		if n.Add(1) == 1 {
			return errors.New("probe failed for " + tok)
		}
		return nil
	}, &remembered)
	sr.stale.Store(true)
	_, done := runRefresher(t, rf)
	waitFor(t, "swap", func() bool { return len(remembered.all()) == 1 })
	_ = done
	check("log", sink.text())

	// The status report is the relay snapshot; it has no field that can hold a token.
	report, err := json.Marshal(sr.relay.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	check("report", string(report))
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
	buildOut  string
)

// accelerator builds the real executable once per test process.
func accelerator(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "tf-accel-bin")
		if err != nil {
			buildErr = err
			return
		}
		name := "accel"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		builtBin = filepath.Join(dir, name)
		out, err := exec.Command("go", "build", "-o", builtBin, ".").CombinedOutput()
		buildOut, buildErr = string(out), err
	})
	if buildErr != nil {
		t.Fatalf("go build: %v\n%s", buildErr, buildOut)
	}
	return builtBin
}

func runAccelerator(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(accelerator(t), args...)
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code = 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("accelerator did not exit\n%s%s", o.String(), e.String())
	}
	return o.String(), e.String(), code
}

func TestTokenFileFlagRemoved(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte(tokenA), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runAccelerator(t, "", "--phone-token-file", file, "--check-phone", "--external-tunnel")
	if code != 2 {
		t.Fatalf("exit code %d, want 2 from the flag package\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "phone-token-file") || strings.Contains(stdout+stderr, tokenA) {
		t.Fatalf("unexpected output:\n%s%s", stdout, stderr)
	}
}

func TestBinaryTokenFromStdinOnly(t *testing.T) {
	t.Run("accepted from stdin", func(t *testing.T) {
		var n atomic.Int32
		addr := fakePhone(t, tokenA, &n)
		stdout, stderr, code := runAccelerator(t, tokenA+"\n", "--check-phone", "--external-tunnel", "--phone-proxy", addr, "--report", "")
		if code != 0 || !strings.Contains(stdout, "Verified USB app-path payload") {
			t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
		}
		if strings.Contains(stdout+stderr, tokenA) {
			t.Fatal("token printed")
		}
	})
	t.Run("grouped uppercase input is normalized", func(t *testing.T) {
		var n atomic.Int32
		addr := fakePhone(t, tokenB, &n)
		_, stderr, code := runAccelerator(t, tokenBGroups+"\r\n", "--check-phone", "--external-tunnel", "--phone-proxy", addr)
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
	})
	t.Run("three rejected tokens", func(t *testing.T) {
		var n atomic.Int32
		addr := fakePhone(t, tokenA, &n)
		stdout, stderr, code := runAccelerator(t, strings.Repeat(tokenB+"\n", 5), "--check-phone", "--external-tunnel", "--phone-proxy", addr)
		if code != 1 || n.Load() != 3 {
			t.Fatalf("exit %d after %d probes, want exit 1 after 3\n%s%s", code, n.Load(), stdout, stderr)
		}
		if strings.Contains(stdout+stderr, tokenB) {
			t.Fatal("token printed")
		}
	})
	t.Run("EOF without token", func(t *testing.T) {
		var n atomic.Int32
		addr := fakePhone(t, tokenA, &n)
		_, stderr, code := runAccelerator(t, "", "--check-phone", "--external-tunnel", "--phone-proxy", addr)
		if code != 1 || n.Load() != 0 || !strings.Contains(stderr, "no iPhone session token") {
			t.Fatalf("exit %d probes %d\n%s", code, n.Load(), stderr)
		}
	})
	t.Run("environment is not a token source", func(t *testing.T) {
		var n atomic.Int32
		addr := fakePhone(t, tokenA, &n)
		cmd := exec.Command(accelerator(t), "--check-phone", "--external-tunnel", "--phone-proxy", addr)
		cmd.Env = append(os.Environ(), "TETHERFLOW_PHONE_TOKEN="+tokenA, "PHONE_TOKEN="+tokenA)
		cmd.Stdin = strings.NewReader("")
		if err := cmd.Run(); err == nil || n.Load() != 0 {
			t.Fatalf("environment token was used (probes %d)", n.Load())
		}
	})
}
