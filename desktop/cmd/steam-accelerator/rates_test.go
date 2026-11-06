package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tetherflow-desktop/internal/steamaccel"
)

func TestIntervalMbps(t *testing.T) {
	if got := intervalMbps(100, 150000100, time.Second); got != 1200 {
		t.Fatal(got)
	}
	for _, v := range []float64{intervalMbps(2, 1, time.Second), intervalMbps(0, 1, 0), intervalMbps(0, 1, -time.Second)} {
		if v != 0 {
			t.Fatal(v)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// assignedPrivateIPv4 returns an RFC1918 address that is assigned to an up interface, which is
// what --home-ip validation requires.
func assignedPrivateIPv4(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("no interface list: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil && ip.IsPrivate() {
				return ip.String()
			}
		}
	}
	t.Skip("no private IPv4 assigned to an up interface")
	return ""
}

// The built binary in home mode must reach the listener stage without touching the Apple USB
// service and without prompting for a token: stdin stays open and silent, so a prompt would
// either print its label or block.
func TestHomeModeOpensNoUSBAndPromptsNoToken(t *testing.T) {
	ip := assignedPrivateIPv4(t)
	cmd := exec.Command(accelerator(t), "--uplink-mode", "home", "--home-ip", ip, "--report", t.TempDir()+"/status.json")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() { _ = cmd.Process.Kill(); <-done }()
	deadline := time.After(30 * time.Second)
	for {
		text := out.String()
		if strings.Contains(text, "Listening ") || strings.Contains(text, "cannot bind") {
			break
		}
		select {
		case <-done:
			text = out.String()
			if !strings.Contains(text, "cannot bind") {
				t.Fatalf("exited before the listener stage:\n%s", text)
			}
		case <-deadline:
			t.Fatalf("no listener stage within 30s:\n%s", text)
		case <-time.After(50 * time.Millisecond):
			continue
		}
		break
	}
	text := out.String()
	for _, forbidden := range []string{tokenPromptLabel, "USB", "token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("home mode output mentions %q:\n%s", forbidden, text)
		}
	}
}

// mbpsBytes is the byte count that moves at the given rate for the duration.
func mbpsBytes(mbps float64, d time.Duration) uint64 { return uint64(mbps * 1e6 / 8 * d.Seconds()) }

// feeder drives a tracker with fixed-length intervals at chosen per-path rates.
type feeder struct {
	tr          *concurrencyTracker
	now         time.Time
	home, phone uint64
	step        time.Duration
}

func newFeeder(step time.Duration) *feeder {
	t0 := time.Unix(1_700_000_000, 0)
	return &feeder{tr: newConcurrencyTracker(t0), now: t0, step: step}
}

func (f *feeder) tick(homeMbps, phoneMbps float64) {
	f.now = f.now.Add(f.step)
	f.home += mbpsBytes(homeMbps, f.step)
	f.phone += mbpsBytes(phoneMbps, f.step)
	f.tr.observe(f.now, f.home, f.phone)
}

func TestConcurrencyRequiresSameIntervalAboveFloor(t *testing.T) {
	f := newFeeder(2 * time.Second)
	f.tick(10, 0) // home alone
	if f.tr.bothNow || f.tr.concurrent != 0 {
		t.Fatalf("home alone is concurrent: %+v", f.tr)
	}
	f.tick(5, 5) // both
	if !f.tr.bothNow || f.tr.concurrent != 2*time.Second {
		t.Fatalf("both above the floor not counted: %+v", f.tr)
	}
	f.tick(concurrentFloorMbps, 5) // exactly at the floor is not above it
	if f.tr.bothNow || f.tr.concurrent != 2*time.Second {
		t.Fatalf("a path at the floor counted: %+v", f.tr)
	}
	f.tick(0, 5)
	f.tick(1.2, 1.2)
	if !f.tr.bothNow || f.tr.concurrent != 4*time.Second || f.tr.longest != 2*time.Second {
		t.Fatalf("concurrent=%v longest=%v now=%v", f.tr.concurrent, f.tr.longest, f.tr.bothNow)
	}
	if f.tr.active != 10*time.Second {
		t.Fatalf("active=%v", f.tr.active)
	}
}

// Home moved bytes early and the phone moved bytes later: both counters are non-zero (the old
// both_paths_observed would be true) but the paths were never busy in the same interval.
func TestHistoricalBytesAreNotConcurrent(t *testing.T) {
	f := newFeeder(3 * time.Second)
	for i := 0; i < 5; i++ {
		f.tick(80, 0)
	}
	for i := 0; i < 5; i++ {
		f.tick(0, 80)
	}
	if f.tr.concurrent != 0 || f.tr.longest != 0 || f.tr.bothNow {
		t.Fatalf("sequential traffic counted as concurrent: %+v", f.tr)
	}
	status := steamaccel.Status{Home: steamaccel.PathStatus{Bytes: f.home}, Phone: steamaccel.PathStatus{Bytes: f.phone}}
	r := newStatusReport(f.tr.last, false, steamaccel.UplinkDual, status, 0, 80, f.tr)
	if r.ConcurrentSecs != 0 || !r.BothEver || r.BothActiveNow {
		t.Fatalf("report: concurrent=%v ever=%v now=%v", r.ConcurrentSecs, r.BothEver, r.BothActiveNow)
	}
}

func TestStreakResetsOnGap(t *testing.T) {
	f := newFeeder(time.Second)
	f.tick(5, 5)
	f.tick(5, 5)
	f.tick(5, 0) // gap
	if f.tr.streak != 0 || f.tr.longest != 2*time.Second {
		t.Fatalf("streak=%v longest=%v", f.tr.streak, f.tr.longest)
	}
	f.tick(5, 5)
	f.tick(0, 0) // idle gap
	f.tick(5, 5)
	if f.tr.streak != time.Second || f.tr.longest != 2*time.Second || f.tr.concurrent != 4*time.Second {
		t.Fatalf("streak=%v longest=%v concurrent=%v", f.tr.streak, f.tr.longest, f.tr.concurrent)
	}
	f.tick(5, 5)
	f.tick(5, 5)
	if f.tr.longest != 3*time.Second {
		t.Fatalf("longest=%v", f.tr.longest)
	}
	before := *f.tr
	f.tr.observe(f.now, f.home+1, f.phone+1) // time did not advance
	if *f.tr != before {
		t.Fatal("a zero-length interval changed the tracker")
	}
}

func TestSustainedMbpsExcludesIdleTime(t *testing.T) {
	f := newFeeder(2 * time.Second)
	f.tick(80, 20)
	f.tick(80, 20)
	for i := 0; i < 6; i++ {
		f.tick(0, 0.5) // idle: below the floor
	}
	got := f.tr.sustained()
	near := func(a, b float64) bool { return a > b*0.999 && a < b*1.001 }
	if f.tr.active != 4*time.Second || !near(got.Home, 80) || !near(got.Phone, 20) || !near(got.Combined, 100) {
		t.Fatalf("active=%v sustained=%+v", f.tr.active, got)
	}
	if empty := newFeeder(time.Second).tr.sustained(); empty != (sustainedMbps{}) {
		t.Fatalf("no active time must report zero: %+v", empty)
	}
}

func TestReportFieldsAndNoToken(t *testing.T) {
	relay := newTestRelay(t, tokenA)
	f := newFeeder(time.Second)
	f.tick(10, 10)
	stale := relay.Snapshot()
	stale.PhoneAuth = steamaccel.PhoneAuthStatus{Stale: true, Generation: 2, Rejections: 3, StaleSince: f.now}
	decode := func(r statusReport) map[string]any {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{tokenA, tokenB} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("report contains a token: %s", raw)
			}
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := decode(newStatusReport(f.tr.last, true, steamaccel.UplinkDual, stale, 10, 10, f.tr))
	for _, key := range []string{"uplink_mode", "both_paths_active_now", "concurrent_seconds", "longest_concurrent_seconds",
		"both_paths_ever", "active_seconds", "sustained_mbps", "phone_auth", "home_interval_mbps", "phone_interval_mbps", "uplinks"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing field %q", key)
		}
	}
	if _, ok := m["both_paths_observed"]; ok {
		t.Error("both_paths_observed must be gone: it was a historical flag, not concurrency")
	}
	if m["uplink_mode"] != "dual" || m["both_paths_active_now"] != true || m["concurrent_seconds"] != 1.0 {
		t.Errorf("unexpected values: %v", m)
	}
	sm, _ := m["sustained_mbps"].(map[string]any)
	for _, key := range []string{"home", "phone", "combined"} {
		if _, ok := sm[key]; !ok {
			t.Errorf("sustained_mbps lacks %q", key)
		}
	}
	auth, _ := m["phone_auth"].(map[string]any)
	if auth["stale"] != true || auth["generation"] != 2.0 || auth["rejections"] != 3.0 || auth["stale_since"] == nil || len(auth) != 4 {
		t.Errorf("phone_auth = %v", auth)
	}
	home := decode(newStatusReport(f.tr.last, false, steamaccel.UplinkHomeOnly, relay.Snapshot(), 10, 0, f.tr))
	if v, present := home["phone_auth"]; !present || v != nil || home["uplink_mode"] != "home" {
		t.Errorf("home mode: phone_auth=%v mode=%v", v, home["uplink_mode"])
	}
	if phone := decode(newStatusReport(f.tr.last, false, steamaccel.UplinkPhoneOnly, relay.Snapshot(), 0, 10, f.tr)); phone["uplink_mode"] != "phone" {
		t.Errorf("phone mode: %v", phone["uplink_mode"])
	}
}

func TestParseUplinkMode(t *testing.T) {
	for in, want := range map[string]steamaccel.UplinkMode{"dual": steamaccel.UplinkDual, "home": steamaccel.UplinkHomeOnly, "phone": steamaccel.UplinkPhoneOnly} {
		got, err := parseUplinkMode(in)
		if err != nil || got != want || modeName(got) != in {
			t.Errorf("%q: got %v err %v", in, got, err)
		}
		if name := defaultReportPath(got); !strings.Contains(name, in) {
			t.Errorf("report name %q lacks the mode", name)
		}
	}
	if defaultReportPath(steamaccel.UplinkHomeOnly) == defaultReportPath(steamaccel.UplinkPhoneOnly) || defaultReportPath(steamaccel.UplinkDual) == defaultReportPath(steamaccel.UplinkHomeOnly) {
		t.Error("modes share a report file")
	}
	for _, bad := range []string{"", "both", "homeonly", "phoneonly", "DUAL", " home", "home "} {
		if _, err := parseUplinkMode(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestReportWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	f := newFeeder(time.Second)
	for i := 0; i < 3; i++ {
		r := newStatusReport(f.tr.last, false, steamaccel.UplinkDual, steamaccel.Status{}, float64(i), 0, f.tr)
		if err := writeReport(path, r); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		var back map[string]any
		if err != nil || json.Unmarshal(raw, &back) != nil || back["home_interval_mbps"] != float64(i) {
			t.Fatalf("round %d: %v %s", i, err, raw)
		}
	}
	if err := writeReport(filepath.Join(dir, "missing", "status.json"), statusReport{}); err == nil {
		t.Fatal("write into a missing directory succeeded")
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(blocker, statusReport{}); err == nil {
		t.Fatal("rename over a directory succeeded")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "status.json" && e.Name() != "blocker" {
			t.Errorf("leftover %q", e.Name())
		}
	}
}

func TestHomeModeRejectsPhoneFlagsAndBadMode(t *testing.T) {
	for _, args := range [][]string{
		{"--uplink-mode", "home", "--device-udid", "x"},
		{"--uplink-mode", "home", "--external-tunnel"},
		{"--uplink-mode", "home", "--check-phone"},
		{"--uplink-mode", "home", "--phone-proxy", "127.0.0.1:9"},
		{"--uplink-mode", "homeonly"},
	} {
		stdout, stderr, code := runAccelerator(t, "", args...)
		if code != 1 || strings.Contains(stdout+stderr, tokenPromptLabel) || !strings.Contains(stderr, "--") {
			t.Errorf("%v: exit %d\n%s%s", args, code, stdout, stderr)
		}
	}
}
