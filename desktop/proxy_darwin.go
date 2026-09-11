//go:build darwin

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

func getActiveNetworkServices() []string {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return []string{"Wi-Fi"}
	}

	lines := strings.Split(string(out), "\n")
	var services []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "*") {
			continue
		}
		services = append(services, line)
	}
	if len(services) == 0 {
		return []string{"Wi-Fi"}
	}
	return services
}

func EnableSystemProxy(ip string, port int) error {
	portStr := strconv.Itoa(port)
	services := getActiveNetworkServices()

	for _, svc := range services {
		exec.Command("networksetup", "-setwebproxy", svc, ip, portStr).Run()
		exec.Command("networksetup", "-setsecurewebproxy", svc, ip, portStr).Run()
		exec.Command("networksetup", "-setwebproxystate", svc, "on").Run()
		exec.Command("networksetup", "-setsecurewebproxystate", svc, "on").Run()
	}
	return nil
}

func DisableSystemProxy() error {
	services := getActiveNetworkServices()
	for _, svc := range services {
		exec.Command("networksetup", "-setwebproxystate", svc, "off").Run()
		exec.Command("networksetup", "-setsecurewebproxystate", svc, "off").Run()
	}
	return nil
}

func NotifyUser(title, message string) {
	script := strings.ReplaceAll(message, `"`, `\"`)
	exec.Command("osascript", "-e", `display notification "`+script+`" with title "`+title+`"`).Run()
}

func GetSystemDefaultGateways() []string {
	out, err := exec.Command("sh", "-c", "route -n get default | awk '/gateway/{print $2}'").Output()
	if err != nil {
		return nil
	}
	var gws []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		gw := strings.TrimSpace(line)
		if gw != "" && !seen[gw] {
			seen[gw] = true
			gws = append(gws, gw)
		}
	}
	return gws
}

func RegisterExitHandler(onExit func()) {
}

