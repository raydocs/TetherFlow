//go:build windows

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// EnsureAutoStart checks if the executable is in AppData, copies it there if not,
// and registers it in the Windows Startup registry (Run key).
func EnsureAutoStart() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}

	appData := os.Getenv("APPDATA")
	if appData == "" {
		return
	}

	targetDir := filepath.Join(appData, "TetherFlow")
	targetExe := filepath.Join(targetDir, "tetherflow-win.exe")

	// If not running from permanent target location, install ourselves there
	cleanExe := filepath.Clean(exePath)
	cleanTarget := filepath.Clean(targetExe)

	if cleanExe != cleanTarget {
		_ = os.MkdirAll(targetDir, 0755)

		src, err := os.Open(cleanExe)
		if err == nil {
			dst, err := os.OpenFile(cleanTarget, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err == nil {
				_, _ = io.Copy(dst, src)
				dst.Close()
			}
			src.Close()
		}
	}

	// Register in Windows Run key (HKCU\Software\Microsoft\Windows\CurrentVersion\Run)
	regKey := `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	_ = exec.Command("reg", "add", regKey, "/v", "TetherFlow", "/t", "REG_SZ", "/d", `"`+cleanTarget+`"`, "/f").Run()
}
