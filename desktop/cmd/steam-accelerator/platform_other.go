//go:build !windows

package main

import (
	"errors"
	"os/exec"
)

func hostsPath() (string, error) {
	return "", errors.New("Steam discovery installation is Windows-only; host tests do not change system files")
}
func flushDNS() error { return errors.New("Windows-only") }
func prepareGuardian(cmd *exec.Cmd) (func(), error) {
	return func() {}, errors.New("guardian is Windows-only")
}
func guard(pid int, lease string, handle uint64, recoveryLog string) error {
	return errors.New("guardian is Windows-only")
}
func guardPath(pid int, lease string, handle uint64, hostsFile string, flush func() error, recoveryLog string) error {
	return errors.New("guardian is Windows-only")
}
