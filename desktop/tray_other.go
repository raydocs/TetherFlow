//go:build !windows

package main

type TrayManager struct{}

func StartTray(onExit func(), onModeChange func(string)) *TrayManager {
	return &TrayManager{}
}

func (tm *TrayManager) SetStatus(status, ip string) {
}

func (tm *TrayManager) NotifyBalloon(title, info string, isWarn bool) {
}

func (tm *TrayManager) SelectMode(mode string) {
}
