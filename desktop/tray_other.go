//go:build !windows

package main

type TrayManager struct{}

func StartTray(onExit func()) *TrayManager {
	return &TrayManager{}
}

func (tm *TrayManager) SetStatus(status, ip string) {
}

func (tm *TrayManager) NotifyBalloon(title, info string, isWarn bool) {
}
