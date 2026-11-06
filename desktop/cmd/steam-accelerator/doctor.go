package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tetherflow-desktop/internal/steamaccel"
	"tetherflow-desktop/internal/steamsetup"
	"tetherflow-desktop/internal/usbmux"
)

// Doctor is read-only: it lists, inspects and, when --home-ip is given, binds TCP80/443
// for an instant to prove they are free. It writes no hosts entry, no file, no setting.
// It prints no serial and needs no token. A FAIL is exit status 2.

const (
	statusOK   = "OK"
	statusWarn = "WARN"
	statusFail = "FAIL"
)

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	Result     string        `json:"result"`
	UplinkMode string        `json:"uplink_mode"`
	Checks     []doctorCheck `json:"checks"`
	Notes      []string      `json:"notes"`
}

type doctorOptions struct {
	Mode           steamaccel.UplinkMode
	HomeIP         string
	HTTPOnly       bool
	ExternalTunnel bool
	Serial         string
	JSON           bool
}

type ifaceAddr struct {
	Name  string
	Index int
	IP    net.IP
}

type doctorEnv struct {
	interfaces  func() ([]ifaceAddr, error)
	listUSB     func(context.Context) ([]usbmux.Device, error)
	listen      func(network, address string) (net.Listener, error)
	hostsPath   func() (string, error)
	inspect     func(string) (steamsetup.Inspection, error)
	recoverable func(string) bool
	exe         string
}

func prodDoctorEnv() doctorEnv {
	exe := "tetherflow-steam-accelerator.exe"
	if p, err := os.Executable(); err == nil {
		exe = filepath.Base(p)
	}
	return doctorEnv{interfaces: activeIPv4, listUSB: listUSBDevices, listen: net.Listen, hostsPath: hostsPath, inspect: steamsetup.Inspect, recoverable: steamsetup.Recoverable, exe: exe}
}

func activeIPv4() ([]ifaceAddr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []ifaceAddr
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ip, _, e := net.ParseCIDR(a.String()); e == nil && ip.To4() != nil {
				out = append(out, ifaceAddr{Name: iface.Name, Index: iface.Index, IP: ip})
			}
		}
	}
	return out, nil
}

// doctorExit is returned when any check failed; main turns it into exit status 2.
type doctorExit struct{ fails int }

func (e doctorExit) Error() string { return fmt.Sprintf("doctor: %d check(s) FAILED", e.fails) }
func (e doctorExit) ExitCode() int { return 2 }

func runDoctor(ctx context.Context, opts doctorOptions, env doctorEnv, w io.Writer) error {
	report := buildDoctorReport(ctx, opts, env)
	if opts.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		printDoctorReport(w, report)
	}
	fails := 0
	for _, c := range report.Checks {
		if c.Status == statusFail {
			fails++
		}
	}
	if fails > 0 {
		return doctorExit{fails}
	}
	return nil
}

func buildDoctorReport(ctx context.Context, opts doctorOptions, env doctorEnv) doctorReport {
	r := doctorReport{UplinkMode: opts.Mode.String(), Notes: []string{}}
	add := func(name, status, format string, args ...any) {
		r.Checks = append(r.Checks, doctorCheck{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)})
	}
	usesPhone := opts.Mode != steamaccel.UplinkHomeOnly

	homeOK := false
	addrs, err := env.interfaces()
	switch {
	case err != nil:
		add("interfaces", statusFail, "cannot list network interfaces: %v", err)
	case len(addrs) == 0:
		add("interfaces", statusFail, "no active non-loopback IPv4 interface")
	default:
		var parts []string
		for _, a := range addrs {
			parts = append(parts, fmt.Sprintf("%s %s (index %d)", a.Name, a.IP, a.Index))
		}
		detail := strings.Join(parts, "; ")
		if opts.HomeIP == "" {
			add("interfaces", statusOK, "%s; pass the home Wi-Fi address (not iPhone Ethernet) as --home-ip", detail)
			break
		}
		home := net.ParseIP(opts.HomeIP)
		assigned := false
		for _, a := range addrs {
			assigned = assigned || home != nil && a.IP.Equal(home)
		}
		switch {
		case home == nil || home.To4() == nil || !home.IsPrivate():
			add("interfaces", statusFail, "--home-ip must be the RFC1918 IPv4 assigned to your home Wi-Fi; found: %s", detail)
		case !assigned:
			add("interfaces", statusFail, "--home-ip %s is not assigned to an active interface; found: %s", opts.HomeIP, detail)
		default:
			homeOK = true
			add("interfaces", statusOK, "home IP %s is assigned; found: %s", opts.HomeIP, detail)
		}
	}

	switch {
	case !usesPhone:
		r.Notes = append(r.Notes, "Home-only mode: the Apple USB service, the iPhone and the session token are not used or checked.")
	case opts.ExternalTunnel:
		r.Notes = append(r.Notes, "External tunnel: the built-in Apple USB service is not used or checked.")
	default:
		r.Checks = append(r.Checks, usbChecks(ctx, opts, env)...)
	}

	if opts.HomeIP == "" {
		r.Notes = append(r.Notes, "TCP80/443 bind was not tested: pass --home-ip to test it.")
	} else if homeOK {
		ports := []string{"80"}
		if !opts.HTTPOnly {
			ports = append(ports, "443")
		}
		var bad []string
		for _, p := range ports {
			addr := net.JoinHostPort(opts.HomeIP, p)
			l, e := env.listen("tcp4", addr)
			if e != nil {
				bad = append(bad, fmt.Sprintf("%s: %v", addr, e))
				continue
			}
			l.Close()
		}
		if len(bad) > 0 {
			add("ports", statusFail, "cannot bind: %s; free the port or use --http-only only if intended", strings.Join(bad, "; "))
		} else {
			add("ports", statusOK, "TCP %s can be bound on %s", strings.Join(ports, "/"), opts.HomeIP)
		}
	}

	r.Checks = append(r.Checks, hostsCheck(env))
	if usesPhone {
		r.Notes = append(r.Notes, "Keep the signed TetherFlow iOS app in the foreground and unlocked, tap Start, and type the session token at the hidden prompt. Doctor cannot see the app.")
	}
	r.Notes = append(r.Notes, "Doctor proves local facts only. USB link metadata is not throughput; iPhone cellular throughput and Steam adoption cannot be established here, and startup's cellular probe shows reachability, never speed.")
	r.Result = statusOK
	for _, c := range r.Checks {
		if c.Status == statusFail {
			r.Result = statusFail
		} else if c.Status == statusWarn && r.Result == statusOK {
			r.Result = statusWarn
		}
	}
	return r
}

func usbChecks(ctx context.Context, opts doctorOptions, env doctorEnv) []doctorCheck {
	var out []doctorCheck
	add := func(name, status, format string, args ...any) {
		out = append(out, doctorCheck{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)})
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	devices, err := env.listUSB(ctx)
	if err != nil {
		add("apple-usb-service", statusFail, "Apple USB service unavailable; install/open Apple Devices, connect and trust the iPhone: %v", err)
		return out
	}
	add("apple-usb-service", statusOK, "the Apple USB service answered")
	phones := usbPhones(devices)
	chosen := phones
	switch {
	case opts.Serial != "":
		d, e := usbmux.SelectUSB(devices, 0, opts.Serial)
		if e != nil {
			add("usb-devices", statusFail, "--device-udid matches no single USB iPhone (%d USB iPhone(s) attached); run --list-phones", len(phones))
			return out
		}
		chosen = []usbmux.Device{d}
		add("usb-devices", statusOK, "the --device-udid iPhone is attached (id %d)", d.DeviceID)
	case len(phones) == 0:
		add("usb-devices", statusFail, "no USB iPhone found; connect, unlock and trust it (Wi-Fi sync devices are excluded)")
		return out
	case len(phones) > 1:
		add("usb-devices", statusFail, "%d USB iPhones attached; startup needs --device-udid with a serial from --list-phones", len(phones))
		return out
	default:
		add("usb-devices", statusOK, "1 USB iPhone attached (id %d)", phones[0].DeviceID)
	}
	slow := false
	var speeds []string
	for _, d := range chosen {
		mbps := d.ConnectionSpeed / 1_000_000
		speeds = append(speeds, fmt.Sprintf("id %d: %d Mbps", d.DeviceID, mbps))
		slow = slow || mbps <= 480
	}
	if slow {
		add("usb-link-metadata", statusWarn, "the service reports a link of 480 Mbps or less (%s), USB 2.0 class; try another cable or port. This is metadata, not throughput", strings.Join(speeds, "; "))
	} else {
		add("usb-link-metadata", statusOK, "service-reported link %s; metadata, not throughput", strings.Join(speeds, "; "))
	}
	return out
}

func hostsCheck(env doctorEnv) doctorCheck {
	c := doctorCheck{Name: "hosts"}
	path, err := env.hostsPath()
	if err != nil {
		c.Status, c.Detail = statusWarn, fmt.Sprintf("hosts file not inspected: %v", err)
		return c
	}
	in, err := env.inspect(path)
	if err != nil {
		c.Status, c.Detail = statusFail, fmt.Sprintf("cannot inspect hosts file %s: %v", path, err)
		return c
	}
	c.Status = statusOK
	var details []string
	raise := func(status string) {
		if status == statusFail || c.Status == statusOK {
			c.Status = status
		}
	}
	if in.Malformed && !(len(in.PendingJournals) > 0 && env.recoverable != nil && env.recoverable(path)) {
		raise(statusFail)
		details = append(details, "a TetherFlow block is malformed; inspect the hosts file by hand, it is not touched")
	}
	if in.ForeignOverride {
		raise(statusFail)
		details = append(details, "another tool already overrides lancache.steamcontent.com; it is not touched")
	}
	for _, l := range in.Leases {
		raise(statusWarn)
		details = append(details, fmt.Sprintf("session entry lease=%s ip=%s exists; if no accelerator is running, recover with: %s --recover-lease %s", l.Lease, l.IP, env.exe, l.Lease))
	}
	for _, j := range in.PendingJournals {
		raise(statusWarn)
		details = append(details, fmt.Sprintf("unfinished hosts journal %s; the next --enable-steam resolves it, or refuses and leaves it", j))
	}
	if len(details) == 0 {
		c.Detail = "no TetherFlow entry, foreign override or pending journal in " + path
	} else {
		c.Detail = strings.Join(details, "; ")
	}
	return c
}

func printDoctorReport(w io.Writer, r doctorReport) {
	fmt.Fprintf(w, "TetherFlow Steam doctor (uplink-mode %s): read-only, no hosts, network or account change\n", r.UplinkMode)
	for _, c := range r.Checks {
		fmt.Fprintf(w, "[%-4s] %s: %s\n", c.Status, c.Name, c.Detail)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	fmt.Fprintf(w, "Result: %s\n", r.Result)
}

var errDoctorJSON = errors.New("--json is only valid with --doctor")
