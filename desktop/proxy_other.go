//go:build !windows && !darwin

package main

import (
	"fmt"
	"os/exec"
	"strconv"
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
