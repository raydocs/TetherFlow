//go:build windows

package main

import (
	"os/exec"
	"strings"
)

func DetectLinkDetails(ip string) (linkTitle string, isUsb3 bool, isWarn bool) {
	if ip != "127.0.0.1" {
		return "5G Wi-Fi 6 满速无线热点", true, false
	}

	// For 127.0.0.1 (USB connection), detect if USB 3.0 or USB 2.0
	// Query Windows PnP Device Property for DEVPKEY_Device_Speed
	// Speed 4 = SuperSpeed (5Gbps), Speed 5 = SuperSpeedPlus (10Gbps), Speed 3 = HighSpeed (480Mbps)
	psCmd := `$d = Get-PnpDevice -Class USB -ErrorAction SilentlyContinue | Where-Object { $_.FriendlyName -like '*SAMSUNG*' -and $_.Status -eq 'OK' }; if ($d) { $s = (Get-PnpDeviceProperty -InstanceId $d[0].InstanceId -KeyName 'DEVPKEY_Device_Speed' -ErrorAction SilentlyContinue).Data; if ($s -ge 4) { 'USB3' } else { 'USB2' } } else { '' }`
	out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).Output()
	if err == nil {
		res := strings.TrimSpace(string(out))
		if res == "USB3" {
			return "USB 3.0 极速中继 (5 Gbps 满血 1,200 Mbps)", true, false
		} else if res == "USB2" {
			return "USB 2.0 降级中继 (480 Mbps 慢速 250 Mbps)", false, true
		}
	}

	return "USB 极速中继", true, false
}
