package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"tetherflow-desktop/internal/steamaccel"
)

// The phone session token reaches this process only through the executable's own
// prompt on stdin. It is never a flag, an environment variable or a file, and no
// message in this file includes it.

const (
	maxTokenLine         = 4096
	startupTokenAttempts = 3
	probeBytes           = 1024
	probeTimeout         = 30 * time.Second
	refreshPoll          = 250 * time.Millisecond
	tokenPromptLabel     = "iPhone session token (hidden): "
)

var errTokenLineTooLong = errors.New("token input is longer than 4 KiB and was discarded")

// readTokenLine reads one line, trims surrounding white space and returns it. A
// line longer than 4 KiB is consumed up to its newline (or EOF) without being
// kept or logged and yields errTokenLineTooLong, so the next call starts on the
// next line. EOF before any byte is io.EOF; a final line without a newline is
// returned normally.
func readTokenLine(r *bufio.Reader) (string, error) {
	var buf []byte
	total := 0
	for {
		chunk, err := r.ReadSlice('\n')
		total += len(chunk)
		if len(buf) <= maxTokenLine {
			buf = append(buf, chunk...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && err != io.EOF {
			return "", err
		}
		if total == 0 {
			return "", io.EOF
		}
		line := strings.TrimRight(string(buf), "\r\n")
		if total > maxTokenLine+2 || len(line) > maxTokenLine {
			return "", errTokenLineTooLong
		}
		return strings.TrimSpace(line), nil
	}
}

// withHiddenInput runs fn with console echo off on f and restores the previous
// mode afterwards, also when fn panics. For a stdin that is not a console it
// only runs fn. If echo cannot be turned off on a real console, fn is not run.
func withHiddenInput(f *os.File, fn func()) (err error) {
	restore, err := hideInput(f)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := restore(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	fn()
	return nil
}

// tokenSource owns the single buffered reader over stdin, shared by the startup
// prompt and the refresh goroutine, and remembers every accepted token so the
// recovery log can redact them.
type tokenSource struct {
	in      *bufio.Reader
	console *os.File
	out     io.Writer
	mu      sync.Mutex
	restore func() error
	known   []string
}

func newTokenSource(stdin *os.File, out io.Writer) *tokenSource {
	return &tokenSource{in: bufio.NewReader(stdin), console: stdin, out: out}
}

// read prompts and reads one line with echo off.
func (s *tokenSource) read() (string, error) {
	restore, err := hideInput(s.console)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.restore = restore
	s.mu.Unlock()
	defer s.Close()
	fmt.Fprint(s.out, tokenPromptLabel)
	line, rerr := readTokenLine(s.in)
	fmt.Fprintln(s.out) // the Enter key was not echoed
	return line, rerr
}

// Close restores console echo if a read is in progress. Safe to call repeatedly
// and at shutdown while another goroutine is blocked in read.
func (s *tokenSource) Close() {
	s.mu.Lock()
	restore := s.restore
	s.restore = nil
	s.mu.Unlock()
	if restore != nil {
		_ = restore()
	}
}

func (s *tokenSource) remember(token string) {
	s.mu.Lock()
	s.known = append(s.known, token)
	s.mu.Unlock()
}

// secrets returns every token accepted so far, for redaction.
func (s *tokenSource) secrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.known...)
}

// errPhoneRejected marks a phone answer of 407. checkPhone formats its status
// error as "phone diagnostic HTTP 407 ..."; TestProbeClassifiesRejection runs the
// real checkPhone against a 407 server so a change to that text fails a test.
var errPhoneRejected = errors.New("phone rejected the session token")

// probePhone verifies a candidate token with the authenticated diagnostic
// download (the same check as the startup preflight). R12 owns the full
// preflight, including GET /diagnostic/info protocol >= 2; it must be added here
// and must stay authenticated: a probe may never be weakened to an anonymous one.
func probePhone(ctx context.Context, addr, token string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, err := checkPhone(ctx, addr, token, probeBytes)
	if err != nil && strings.Contains(err.Error(), "HTTP 407") {
		return errPhoneRejected
	}
	return err
}

// acquireStartupToken prompts until the phone accepts a token. Unreadable,
// malformed and rejected (407) input each use one of three attempts. Any other
// probe failure (phone absent, tunnel down) is not the token's fault and ends
// startup at once. EOF on stdin ends startup: no token, no relay.
func acquireStartupToken(read func() (string, error), probe func(string) error, logf func(string, ...any)) (string, error) {
	for attempt := 1; attempt <= startupTokenAttempts; attempt++ {
		raw, err := read()
		switch {
		case errors.Is(err, io.EOF):
			return "", errors.New("no iPhone session token on stdin; run the executable in a console or pipe the token to it")
		case errors.Is(err, errTokenLineTooLong):
			logf("Token input too long and discarded (%d of %d attempts).", attempt, startupTokenAttempts)
			continue
		case err != nil:
			return "", fmt.Errorf("reading iPhone session token: %w", err)
		}
		token, err := steamaccel.NormalizePhoneToken(raw)
		if err != nil {
			logf("Token format invalid: expected 32 characters from the iOS app (%d of %d attempts).", attempt, startupTokenAttempts)
			continue
		}
		err = probe(token)
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, errPhoneRejected) {
			return "", fmt.Errorf("iPhone USB application-path preflight failed; no settings changed: %w", redact(err, raw, token))
		}
		logf("iPhone rejected the token: use the one currently shown in the foreground app (%d of %d attempts).", attempt, startupTokenAttempts)
	}
	return "", fmt.Errorf("no valid iPhone session token after %d attempts; no settings changed", startupTokenAttempts)
}

// refresher turns a phone 407 on the running relay into one prompt per token
// generation. It is the only goroutine that reads stdin after startup.
type refresher struct {
	status   func() steamaccel.PhoneAuthStatus
	read     func() (string, error)
	probe    func(ctx context.Context, token string) error
	set      func(token string) error
	remember func(token string)
	logf     func(string, ...any)
	poll     time.Duration
}

func (rf *refresher) run(ctx context.Context) {
	poll := rf.poll
	if poll <= 0 {
		poll = refreshPoll
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var prompted uint64
	havePrompted := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		st := rf.status()
		if !st.Stale || (havePrompted && st.Generation == prompted) {
			continue
		}
		prompted, havePrompted = st.Generation, true
		rf.logf("PHONE TOKEN REJECTED (generation %d): the relay now serves from the home path only. Type the current token from the iOS app; input is hidden.", st.Generation)
		if !rf.refreshOnce(ctx) {
			return
		}
	}
}

// refreshOnce reads candidates until one is accepted. It returns false when
// refresh is disabled (stdin ended or failed) or ctx is done.
func (rf *refresher) refreshOnce(ctx context.Context) bool {
	for ctx.Err() == nil {
		raw, err := rf.read()
		switch {
		case errors.Is(err, io.EOF):
			rf.logf("stdin closed: token refresh disabled. The relay keeps running; restart to enter a new token.")
			return false
		case errors.Is(err, errTokenLineTooLong):
			rf.logf("Token input too long and discarded; type the token again.")
			continue
		case err != nil:
			rf.logf("stdin unreadable: token refresh disabled. The relay keeps running; restart to enter a new token.")
			return false
		}
		token, err := steamaccel.NormalizePhoneToken(raw)
		if err != nil {
			rf.logf("Token format invalid: expected 32 characters; type it again.")
			continue
		}
		if err := rf.probe(ctx, token); err != nil {
			if errors.Is(err, errPhoneRejected) {
				rf.logf("iPhone rejected that token; the old token stays active. Type the one currently shown in the app.")
			} else {
				rf.logf("Could not verify the token with the iPhone (%v); the old token stays active. Check USB and the foreground app, then type it again.", redact(err, raw, token))
			}
			continue
		}
		if err := rf.set(token); err != nil {
			rf.logf("Token refresh failed (%v); the old token stays active.", redact(err, raw, token))
			continue
		}
		rf.remember(token)
		rf.logf("Phone token refreshed; the phone path is serving again.")
		return true
	}
	return false
}

// redact removes secrets from an error before it is logged or returned. Errors
// from the probe and the relay do not contain tokens; this guards the invariant.
func redact(err error, secrets ...string) error {
	text := err.Error()
	changed := false
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			text = strings.ReplaceAll(text, s, "[redacted]")
			changed = true
		}
	}
	if !changed {
		return err
	}
	return errors.New(text)
}
