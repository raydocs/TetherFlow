//go:build !windows

package main

import "os"

// hideInput is a no-op off Windows: the accelerator's Steam integration is
// Windows-only, and the non-Windows build exists for host tests. A terminal
// there echoes what is typed. The restore function is always non-nil.
func hideInput(f *os.File) (func() error, error) {
	return func() error { return nil }, nil
}
