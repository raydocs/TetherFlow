package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"tetherflow-desktop/internal/steamaccel"
	"tetherflow-desktop/internal/steamsetup"
	"tetherflow-desktop/internal/usbmux"
)

const (
	r12Lease  = "0123456789abcdef0123456789abcdef"
	r12HomeIP = "192.168.1.10"
)

// fakeUSBMux serves ListDevices for any number of control connections.
func fakeUSBMux(t *testing.T, entries ...string) *usbmux.Client {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	body := `<?xml version="1.0" encoding="UTF-8"?>` + "\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		`<plist version="1.0"><dict><key>DeviceList</key><array>` + strings.Join(entries, "") + `</array></dict></plist>`
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				for {
					var h [16]byte
					if _, err := io.ReadFull(c, h[:]); err != nil {
						return
					}
					if _, err := io.CopyN(io.Discard, c, int64(binary.LittleEndian.Uint32(h[:]))-16); err != nil {
						return
					}
					p := make([]byte, 16+len(body))
					binary.LittleEndian.PutUint32(p, uint32(len(p)))
					binary.LittleEndian.PutUint32(p[4:], 1)
					binary.LittleEndian.PutUint32(p[8:], 8)
					binary.LittleEndian.PutUint32(p[12:], binary.LittleEndian.Uint32(h[12:]))
					copy(p[16:], body)
					if _, err := c.Write(p); err != nil {
						return
					}
				}
			}()
		}
	}()
	client, err := usbmux.NewClient(usbmux.Config{Network: "tcp", Address: l.Addr().String(), HandshakeTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func usbEntry(id uint32, serial, kind string, speed uint64) string {
	return fmt.Sprintf(`<dict><key>DeviceID</key><integer>%d</integer><key>Properties</key><dict><key>DeviceID</key><integer>%d</integer><key>SerialNumber</key><string>%s</string><key>ConnectionType</key><string>%s</string><key>ConnectionSpeed</key><integer>%d</integer></dict></dict>`, id, id, serial, kind, speed)
}

type doctorFixture struct {
	env       doctorEnv
	hosts     string
	listens   []string
	listeners []net.Listener
	devices   []usbmux.Device
	usbErr    error
	usbCalls  int
	bindErr   map[string]error
}

func newDoctorFixture(t *testing.T) *doctorFixture {
	t.Helper()
	f := &doctorFixture{hosts: filepath.Join(t.TempDir(), "hosts"), bindErr: map[string]error{}}
	if err := os.WriteFile(f.hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.devices = []usbmux.Device{{DeviceID: 4, Serial: "AB-CD-EF", ConnectionType: "USB", ConnectionSpeed: 480_000_000}}
	f.env = doctorEnv{
		interfaces: func() ([]ifaceAddr, error) {
			return []ifaceAddr{{Name: "Wi-Fi", Index: 3, IP: net.ParseIP(r12HomeIP)}}, nil
		},
		listUSB: func(context.Context) ([]usbmux.Device, error) {
			f.usbCalls++
			return f.devices, f.usbErr
		},
		listen: func(network, address string) (net.Listener, error) {
			f.listens = append(f.listens, address)
			if err := f.bindErr[address]; err != nil {
				return nil, err
			}
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			f.listeners = append(f.listeners, l)
			return l, err
		},
		hostsPath: func() (string, error) { return f.hosts, nil },
		inspect:   steamsetup.Inspect,
		exe:       "acc.exe",
	}
	return f
}

func (f *doctorFixture) run(t *testing.T, opts doctorOptions) (doctorReport, string, error) {
	t.Helper()
	var out bytes.Buffer
	opts.JSON = true
	err := runDoctor(context.Background(), opts, f.env, &out)
	var rep doctorReport
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("doctor JSON: %v\n%s", jerr, out.String())
	}
	return rep, out.String(), err
}

func (r doctorReport) status(name string) string {
	for _, c := range r.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return "absent"
}

func exitCode(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

func TestDoctorReportsEachCheck(t *testing.T) {
	f := newDoctorFixture(t)
	rep, _, err := f.run(t, doctorOptions{Mode: steamaccel.UplinkDual, HomeIP: r12HomeIP})
	if err != nil {
		t.Fatalf("WARN only must not fail: %v", err)
	}
	want := []struct{ name, status string }{
		{"interfaces", "OK"}, {"apple-usb-service", "OK"}, {"usb-devices", "OK"},
		{"usb-link-metadata", "WARN"}, // 480 Mbps
		{"ports", "OK"}, {"hosts", "OK"},
	}
	if len(rep.Checks) != len(want) {
		t.Fatalf("checks: %+v", rep.Checks)
	}
	for i, w := range want {
		if rep.Checks[i].Name != w.name || rep.Checks[i].Status != w.status {
			t.Errorf("check %d = %+v, want %s %s", i, rep.Checks[i], w.name, w.status)
		}
	}
	if rep.Result != "WARN" || rep.UplinkMode != "dual" {
		t.Errorf("result %q mode %q", rep.Result, rep.UplinkMode)
	}
	notes := strings.Join(rep.Notes, " ")
	if !strings.Contains(notes, "foreground") || !strings.Contains(notes, "not throughput") {
		t.Errorf("notes lack the foreground reminder or the throughput caveat: %s", notes)
	}
	f.devices[0].ConnectionSpeed = 5_000_000_000
	if rep, _, _ = f.run(t, doctorOptions{Mode: steamaccel.UplinkDual}); rep.status("usb-link-metadata") != "OK" || rep.Result != "OK" {
		t.Errorf("a faster link must be OK: %+v", rep)
	}
	var text bytes.Buffer
	if err := runDoctor(context.Background(), doctorOptions{Mode: steamaccel.UplinkDual, HomeIP: r12HomeIP}, f.env, &text); err != nil || !regexp.MustCompile(`(?m)^\[OK  \] interfaces:`).Match(text.Bytes()) || !strings.Contains(text.String(), "Result: OK") {
		t.Errorf("text report:\n%s", text.String())
	}
}

func TestDoctorExitCodes(t *testing.T) {
	two := append([]usbmux.Device(nil), usbmux.Device{DeviceID: 4, Serial: "aa", ConnectionType: "USB", ConnectionSpeed: 5e9}, usbmux.Device{DeviceID: 5, Serial: "bb", ConnectionType: "USB", ConnectionSpeed: 5e9})
	for _, tc := range []struct {
		name   string
		mutate func(*doctorFixture)
		opts   doctorOptions
		check  string
		fail   bool
	}{
		{"all ok", func(f *doctorFixture) { f.devices[0].ConnectionSpeed = 5e9 }, doctorOptions{Mode: steamaccel.UplinkDual, HomeIP: r12HomeIP}, "", false},
		{"usb service down", func(f *doctorFixture) { f.usbErr = errors.New("refused") }, doctorOptions{Mode: steamaccel.UplinkDual}, "apple-usb-service", true},
		{"no phone", func(f *doctorFixture) { f.devices = nil }, doctorOptions{Mode: steamaccel.UplinkPhoneOnly}, "usb-devices", true},
		{"wifi-only entry", func(f *doctorFixture) { f.devices[0].ConnectionType = "Network" }, doctorOptions{Mode: steamaccel.UplinkDual}, "usb-devices", true},
		{"two phones without serial", func(f *doctorFixture) { f.devices = two }, doctorOptions{Mode: steamaccel.UplinkDual}, "usb-devices", true},
		{"two phones with serial", func(f *doctorFixture) { f.devices = two }, doctorOptions{Mode: steamaccel.UplinkDual, Serial: "bb"}, "", false},
		{"home ip not assigned", func(f *doctorFixture) {}, doctorOptions{Mode: steamaccel.UplinkDual, HomeIP: "192.168.9.9"}, "interfaces", true},
		{"home ip public", func(f *doctorFixture) {}, doctorOptions{Mode: steamaccel.UplinkDual, HomeIP: "8.8.8.8"}, "interfaces", true},
		{"port busy", func(f *doctorFixture) { f.bindErr[r12HomeIP+":443"] = errors.New("in use") }, doctorOptions{Mode: steamaccel.UplinkDual, HomeIP: r12HomeIP}, "ports", true},
		{"port 443 busy but http-only", func(f *doctorFixture) { f.bindErr[r12HomeIP+":443"] = errors.New("in use") }, doctorOptions{Mode: steamaccel.UplinkDual, HomeIP: r12HomeIP, HTTPOnly: true}, "", false},
		{"foreign override", func(f *doctorFixture) {
			os.WriteFile(f.hosts, []byte("10.0.0.1 lancache.steamcontent.com\n"), 0o600)
		}, doctorOptions{Mode: steamaccel.UplinkDual}, "hosts", true},
		{"hosts unreadable", func(f *doctorFixture) { f.hosts = filepath.Join(filepath.Dir(f.hosts), "missing") }, doctorOptions{Mode: steamaccel.UplinkDual}, "hosts", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDoctorFixture(t)
			f.devices[0].ConnectionSpeed = 5e9
			tc.mutate(f)
			rep, _, err := f.run(t, tc.opts)
			if tc.fail {
				if exitCode(err) != 2 || rep.Result != "FAIL" || rep.status(tc.check) != "FAIL" {
					t.Fatalf("want exit 2 and FAIL %s: err=%v rep=%+v", tc.check, err, rep)
				}
				return
			}
			if err != nil || rep.Result == "FAIL" {
				t.Fatalf("unexpected failure: err=%v rep=%+v", err, rep)
			}
		})
	}
}

func TestDoctorDetectsStaleLeaseAndPendingJournal(t *testing.T) {
	f := newDoctorFixture(t)
	block := "127.0.0.1 localhost\n# BEGIN TetherFlow Steam " + r12Lease + "\n192.168.1.10 lancache.steamcontent.com\n# END TetherFlow Steam " + r12Lease + "\n"
	if err := os.WriteFile(f.hosts, []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := f.hosts + ".tetherflow-" + r12Lease + ".journal"
	if err := os.WriteFile(journal, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, _, err := f.run(t, doctorOptions{Mode: steamaccel.UplinkHomeOnly})
	if err != nil || rep.status("hosts") != "WARN" {
		t.Fatalf("a recoverable lease and journal are WARN: err=%v %+v", err, rep.Checks)
	}
	var detail string
	for _, c := range rep.Checks {
		if c.Name == "hosts" {
			detail = c.Detail
		}
	}
	for _, want := range []string{"acc.exe --recover-lease " + r12Lease, "ip=192.168.1.10", filepath.Base(journal)} {
		if !strings.Contains(detail, want) {
			t.Errorf("hosts detail lacks %q: %s", want, detail)
		}
	}
	os.WriteFile(f.hosts, []byte("# BEGIN TetherFlow Steam "+r12Lease+"\n1.1.1.1 evil.example\n# END TetherFlow Steam "+r12Lease+"\n"), 0o600)
	if rep, _, err = f.run(t, doctorOptions{Mode: steamaccel.UplinkHomeOnly}); exitCode(err) != 2 || rep.status("hosts") != "FAIL" {
		t.Errorf("a modified TetherFlow block must FAIL: %v %+v", err, rep.Checks)
	}
}

func TestDoctorHasNoSideEffects(t *testing.T) {
	f := newDoctorFixture(t)
	dir := filepath.Dir(f.hosts)
	snapshot := func() string {
		var b strings.Builder
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			info, _ := e.Info()
			data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			fmt.Fprintf(&b, "%s %d %d %x\n", e.Name(), info.Size(), info.ModTime().UnixNano(), data)
		}
		return b.String()
	}
	before := snapshot()
	if _, _, err := f.run(t, doctorOptions{Mode: steamaccel.UplinkDual, HomeIP: r12HomeIP}); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("doctor changed the hosts directory:\n%s\n---\n%s", before, after)
	}
	if want := []string{r12HomeIP + ":80", r12HomeIP + ":443"}; strings.Join(f.listens, ",") != strings.Join(want, ",") {
		t.Fatalf("bind tests = %v, want %v", f.listens, want)
	}
	for _, l := range f.listeners {
		if c, err := net.DialTimeout("tcp", l.Addr().String(), 200*time.Millisecond); err == nil {
			c.Close()
			t.Fatal("a doctor bind test left its listener open")
		}
	}
	f.listens = nil
	f.run(t, doctorOptions{Mode: steamaccel.UplinkDual, HomeIP: r12HomeIP, HTTPOnly: true})
	if len(f.listens) != 1 {
		t.Fatalf("--http-only must not test 443: %v", f.listens)
	}
	f.listens = nil
	f.run(t, doctorOptions{Mode: steamaccel.UplinkDual})
	if len(f.listens) != 0 {
		t.Fatalf("no --home-ip, no bind: %v", f.listens)
	}
}

func TestDoctorHomeModeNeedsNoUSB(t *testing.T) {
	f := newDoctorFixture(t)
	f.usbErr = errors.New("no Apple USB service on this PC")
	for _, external := range []bool{false, true} {
		mode := steamaccel.UplinkHomeOnly
		if external {
			mode = steamaccel.UplinkDual
		}
		f.usbCalls = 0
		rep, _, err := f.run(t, doctorOptions{Mode: mode, HomeIP: r12HomeIP, ExternalTunnel: external})
		if err != nil || f.usbCalls != 0 {
			t.Fatalf("external=%v: USB queried %d times, err=%v", external, f.usbCalls, err)
		}
		for _, name := range []string{"apple-usb-service", "usb-devices", "usb-link-metadata"} {
			if rep.status(name) != "absent" {
				t.Errorf("external=%v: %s must not be checked", external, name)
			}
		}
	}
	rep, _, err := f.run(t, doctorOptions{Mode: steamaccel.UplinkDual})
	if exitCode(err) != 2 || rep.status("apple-usb-service") != "FAIL" {
		t.Fatalf("dual mode must still need the USB service: %v", err)
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "steam-tools", "Start-SteamAccelerator.ps1"))
	if err != nil || !strings.Contains(string(script), "'--doctor', '--uplink-mode', $uplinkMode") || !strings.Contains(string(script), "Doctor reported a FAIL") {
		t.Errorf("the launcher must run the doctor with the selected mode and stop on its failure: %v", err)
	}
}

func TestDoctorPrintsNoSerial(t *testing.T) {
	f := newDoctorFixture(t)
	f.devices[0].Serial = "SECRET-Serial-123"
	for _, asJSON := range []bool{true, false} {
		var out bytes.Buffer
		if err := runDoctor(context.Background(), doctorOptions{Mode: steamaccel.UplinkDual, JSON: asJSON}, f.env, &out); err != nil {
			t.Fatal(err)
		}
		if s := strings.ToLower(out.String()); strings.Contains(s, "secretserial123") || strings.Contains(s, "secret-serial-123") {
			t.Fatalf("doctor printed a serial (json=%v):\n%s", asJSON, out.String())
		}
	}
}

func TestListPhonesSerialRoundTripsToSelectUSB(t *testing.T) {
	testUSBClient = fakeUSBMux(t,
		usbEntry(5, "AB-CD-EF", "USB", 480_000_000),
		usbEntry(6, "ab-cd-ef", "Network", 0),
		usbEntry(7, "ZZ-99", "USB", 5_000_000_000))
	t.Cleanup(func() { testUSBClient = nil })
	var out bytes.Buffer
	if err := listPhones(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`^serial=(\S+) id=(\d+) link=(\d+)Mbps$`)
	wantIDs := map[string]string{"abcdef": "5", "zz99": "7"}
	seen := 0
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		m := line.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		seen++
		if wantIDs[m[1]] != m[2] {
			t.Errorf("line %q: serial/id pair is wrong", l)
		}
		devices, err := testUSBClient.ListUSB(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		d, err := usbmux.SelectUSB(devices, 0, m[1])
		if err != nil || fmt.Sprint(d.DeviceID) != m[2] {
			t.Errorf("--device-udid %s does not select id %s: %v %+v", m[1], m[2], err, d)
		}
		endpoint, cleanup, err := startPhoneTunnel(context.Background(), m[1])
		if err != nil || endpoint == "" {
			t.Fatalf("startup rejects the printed serial %q: %v", m[1], err)
		}
		cleanup()
	}
	if seen != 2 || strings.Contains(out.String(), "ID=") {
		t.Fatalf("want exactly the two USB phones in serial=… id=… link=… form:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "not a throughput test") {
		t.Errorf("link must be labelled metadata:\n%s", out.String())
	}
}
