//go:build windows

package main

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

const enableEchoInput = 0x0004

var setConsoleModeProc = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

func setConsoleMode(h syscall.Handle, mode uint32) error {
	r, _, e := setConsoleModeProc.Call(uintptr(h), uintptr(mode))
	if r == 0 {
		return e
	}
	return nil
}

// hideInput clears ENABLE_ECHO_INPUT on f when f is a console input handle and
// returns a restore function that puts the original mode back (idempotent, safe
// to call from another goroutine, for example at shutdown while a read is still
// blocked). When f is not a console (a pipe, a file, NUL) it changes nothing and
// the restore function does nothing. If the echo cannot be cleared on a real
// console it fails closed: the caller must not read the secret.
func hideInput(f *os.File) (func() error, error) {
	noop := func() error { return nil }
	if f == nil {
		return noop, nil
	}
	h := syscall.Handle(f.Fd())
	var original uint32
	if err := syscall.GetConsoleMode(h, &original); err != nil {
		return noop, nil // not a console
	}
	if original&enableEchoInput == 0 {
		return noop, nil
	}
	if err := setConsoleMode(h, original&^enableEchoInput); err != nil {
		return noop, fmt.Errorf("cannot turn off console echo: %w", err)
	}
	var once sync.Once
	var restoreErr error
	return func() error {
		once.Do(func() {
			if restoreErr = setConsoleMode(h, original); restoreErr != nil {
				restoreErr = fmt.Errorf("cannot restore console echo: %w", restoreErr)
			}
		})
		return restoreErr
	}, nil
}
