//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"syscall"
)

var (
	wininet           = syscall.NewLazyDLL("wininet.dll")
	internetSetOption = wininet.NewProc("InternetSetOptionW")
)

const (
	INTERNET_OPTION_SETTINGS_CHANGED = 39
	INTERNET_OPTION_REFRESH          = 37
)

func notifyWindowsProxyChange() {
	internetSetOption.Call(0, uintptr(INTERNET_OPTION_SETTINGS_CHANGED), 0, 0)
	internetSetOption.Call(0, uintptr(INTERNET_OPTION_REFRESH), 0, 0)
}

func EnableSystemProxy(ip string, port int) error {
	addr := fmt.Sprintf("%s:%d", ip, port)
	override := "<local>;localhost;127.*;10.*;192.168.*;172.16.*"

	regKey := `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`

	cmd1 := exec.Command("reg", "add", regKey, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "1", "/f")
	if err := cmd1.Run(); err != nil {
		return err
	}

	cmd2 := exec.Command("reg", "add", regKey, "/v", "ProxyServer", "/t", "REG_SZ", "/d", addr, "/f")
	if err := cmd2.Run(); err != nil {
		return err
	}

	cmd3 := exec.Command("reg", "add", regKey, "/v", "ProxyOverride", "/t", "REG_SZ", "/d", override, "/f")
	cmd3.Run()

	notifyWindowsProxyChange()
	return nil
}

func DisableSystemProxy() error {
	regKey := `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`

	cmd := exec.Command("reg", "add", regKey, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "0", "/f")
	if err := cmd.Run(); err != nil {
		return err
	}

	notifyWindowsProxyChange()
	return nil
}
