//go:build !windows

package main

func DetectLinkDetails(ip string) (linkTitle string, isUsb3 bool, isWarn bool) {
	if ip == "127.0.0.1" {
		return "USB 3.0 极速中继 (5 Gbps)", true, false
	}
	return "5G Wi-Fi 6 满速无线热点", true, false
}
