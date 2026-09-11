//go:build !windows && !darwin

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func EnableSystemProxy(ip string, port int) error {
	portStr := strconv.Itoa(port)
	exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "'manual'").Run()
	exec.Command("gsettings", "set", "org.gnome.system.proxy.http", "host", fmt.Sprintf("'%s'", ip)).Run()
	exec.Command("gsettings", "set", "org.gnome.system.proxy.http", "port", portStr).Run()
	exec.Command("gsettings", "set", "org.gnome.system.proxy.https", "host", fmt.Sprintf("'%s'", ip)).Run()
	exec.Command("gsettings", "set", "org.gnome.system.proxy.https", "port", portStr).Run()
	return nil
}

func DisableSystemProxy() error {
	exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "'none'").Run()
	return nil
}

func NotifyUser(title, message string) {
}

func GetSystemDefaultGateways() []string {
	out, err := exec.Command("sh", "-c", "ip route show default | awk '{print $3}'").Output()
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

