//go:build windows

package main

// Windows file modes carry no group/other bits, so the permission assertion is skipped.
func runtimeSupportsModes() bool { return false }
