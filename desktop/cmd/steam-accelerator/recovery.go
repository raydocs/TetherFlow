package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tetherflow-desktop/internal/steamsetup"
)

// Budgets from the hosts-change contract: the guardian keeps trying for five minutes,
// main-process cleanup for fifteen seconds before the guardian takes over.
const (
	guardianBudget    = 5 * time.Minute
	mainCleanupBudget = 15 * time.Second
	flushAttempts     = 3
)

// Test seams; production values never change.
var (
	flushRetryDelay = 500 * time.Millisecond
	// reclaimProofPort is the port whose ownership proves a stale owner is gone.
	reclaimProofPort = "80"
)

// defaultRecoveryLog sits next to the status report, or in the working directory without one.
func defaultRecoveryLog(report string) string {
	dir := "."
	if report != "" {
		dir = filepath.Dir(report)
	}
	p := filepath.Join(dir, "tetherflow-recovery.log")
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// writeRecoveryLog appends one line holding the lease and the error. A guardian has no
// console, so this file is how a failed cleanup is found. Secrets are replaced, never logged.
func writeRecoveryLog(path, lease, phase string, cause error, secrets ...string) {
	if path == "" {
		return
	}
	msg := "nil"
	if cause != nil {
		msg = cause.Error()
	}
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
	}
	msg = strings.NewReplacer("\r", " ", "\n", " ").Replace(msg)
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	if len(lease) != 32 {
		lease = "invalid"
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s lease=%s %s: %s; run --recover-lease %s\n", time.Now().UTC().Format(time.RFC3339), lease, phase, msg, lease)
}

// flushWithRetry runs flush up to flushAttempts times.
func flushWithRetry(ctx context.Context, flush func() error) error {
	var err error
	for i := 0; i < flushAttempts; i++ {
		if err = flush(); err == nil {
			return nil
		}
		if i == flushAttempts-1 {
			break
		}
		t := time.NewTimer(flushRetryDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("DNS flush cancelled: %w (last error: %v)", ctx.Err(), err)
		case <-t.C:
		}
	}
	return fmt.Errorf("DNS flush failed after %d attempts: %w", flushAttempts, err)
}

// cleanupHosts removes lease's entry. Only transient lock errors are retried, within budget;
// refusals and manual-recovery errors return at once. Failures go to recoveryLog.
func cleanupHosts(ctx context.Context, path, lease string, budget time.Duration, flush func() error, recoveryLog string, secrets ...string) error {
	changed, err := steamsetup.UpdateWithRetry(ctx, path, "", lease, steamsetup.RetryPolicy{Total: budget})
	if err != nil {
		writeRecoveryLog(recoveryLog, lease, "hosts cleanup", err, secrets...)
		return err
	}
	if changed && flush != nil {
		if err = flushWithRetry(ctx, flush); err != nil {
			writeRecoveryLog(recoveryLog, lease, "dns flush", err, secrets...)
			return err
		}
	}
	return nil
}

// reclaimStale removes a leftover entry of a dead earlier session. It acts only when the
// caller holds port 80 on homeIP, which proves the old owner is gone, and only for an
// entry whose address equals homeIP. Anything else is refused with the exact command.
func reclaimStale(path, homeIP string, held net.Listener) error {
	in, err := steamsetup.Inspect(path)
	if err != nil {
		return err
	}
	if in.Malformed {
		return errors.New("hosts file has a malformed TetherFlow block; refusing to touch it, inspect it manually")
	}
	if in.ForeignOverride {
		return errors.New("hosts file has a Steam discovery override from another tool; refusing to touch it")
	}
	if len(in.Leases) == 0 {
		return nil
	}
	home := net.ParseIP(homeIP)
	proven := false
	if tcp, ok := heldAddr(held); ok && home != nil && tcp.IP.Equal(home) {
		proven = fmt.Sprint(tcp.Port) == reclaimProofPort
	}
	for _, l := range in.Leases {
		if ip := net.ParseIP(l.IP); !proven || ip == nil || home == nil || !ip.Equal(home) {
			return fmt.Errorf("stale TetherFlow entry %s (%s) is not provably dead; run --recover-lease %s", l.Lease, l.IP, l.Lease)
		}
	}
	for _, l := range in.Leases {
		ctx, cancel := context.WithTimeout(context.Background(), mainCleanupBudget)
		_, err := steamsetup.UpdateWithRetry(ctx, path, "", l.Lease, steamsetup.RetryPolicy{Total: mainCleanupBudget})
		cancel()
		if err != nil {
			return fmt.Errorf("cannot reclaim stale entry %s; run --recover-lease %s: %w", l.Lease, l.Lease, err)
		}
	}
	return nil
}

func heldAddr(l net.Listener) (*net.TCPAddr, bool) {
	if l == nil {
		return nil, false
	}
	a, ok := l.Addr().(*net.TCPAddr)
	return a, ok && a != nil
}
