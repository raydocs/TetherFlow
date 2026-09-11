//go:build windows

package main

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"
)

var (
	wininet               = syscall.NewLazyDLL("wininet.dll")
	internetSetOption     = wininet.NewProc("InternetSetOptionW")
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	setConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
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

func GetSystemDefaultGateways() []string {
	var gws []string
	seen := make(map[string]bool)

	add := func(ipStr string) {
		ipStr = strings.TrimSpace(ipStr)
		if ip := net.ParseIP(ipStr); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			if !seen[ipStr] {
				seen[ipStr] = true
				gws = append(gws, ipStr)
			}
		}
	}

	// 1. Parse route print 0.0.0.0
	out, err := exec.Command("cmd", "/c", "route print 0.0.0.0").Output()
	if err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) >= 4 && fields[0] == "0.0.0.0" && fields[1] == "0.0.0.0" {
				add(fields[2])
			}
		}
	}

	// 2. Fallback: PowerShell Get-NetRoute
	if len(gws) == 0 {
		psOut, err := exec.Command("powershell", "-NoProfile", "-Command", "(Get-NetRoute -DestinationPrefix '0.0.0.0/0').NextHop").Output()
		if err == nil {
			for _, line := range strings.Split(string(psOut), "\n") {
				add(line)
			}
		}
	}

	return gws
}

func RegisterExitHandler(onExit func()) {
	handler := syscall.NewCallback(func(ctrlType uint32) uintptr {
		onExit()
		return 0
	})
	setConsoleCtrlHandler.Call(handler, 1)
}

func NotifyUser(title, message string) {
}

