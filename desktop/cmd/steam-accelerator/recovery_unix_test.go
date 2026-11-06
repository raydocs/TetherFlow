//go:build !windows

package main

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runtimeSupportsModes() bool { return true }

// A foreign flock on the hosts file is the unix analogue of a Windows sharing/lock violation.
func TestCleanupRetriesTransientLock(t *testing.T) {
	p := tempHosts(t, "127.0.0.1 localhost\n"+block(testLease, "192.168.1.5"))
	held, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err = syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
		close(released)
	}()
	var flushed int
	err = cleanupHosts(context.Background(), p, testLease, 10*time.Second, func() error { flushed++; return nil }, "")
	if err != nil {
		t.Fatalf("transient lock was not retried: %v", err)
	}
	select {
	case <-released:
	default:
		t.Fatal("cleanup finished while the lock was still held")
	}
	if flushed != 1 || strings.Contains(readFile(t, p), "TetherFlow") {
		t.Fatalf("flushed=%d hosts=%q", flushed, readFile(t, p))
	}
}

func TestCleanupGivesUpAfterBudget(t *testing.T) {
	p := tempHosts(t, block(testLease, "192.168.1.5"))
	held, _ := os.OpenFile(p, os.O_RDWR, 0)
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	logPath := t.TempDir() + "/r.log"
	start := time.Now()
	err := cleanupHosts(context.Background(), p, testLease, 300*time.Millisecond, func() error { return nil }, logPath)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	if !strings.Contains(readFile(t, logPath), "lease="+testLease) {
		t.Fatal("budget exhaustion not logged")
	}
}
