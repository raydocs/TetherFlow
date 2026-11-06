package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tetherflow-desktop/internal/steamaccel"
)

// concurrentFloorMbps is the rate a path must exceed within one reporting interval to count as
// carrying traffic in that interval. Keep-alives and stray requests stay below it.
const concurrentFloorMbps = 1.0

// intervalMbps converts the downstream bytes that were added to a path's counter between two
// snapshots into Mbps. The relay adds bytes as each write to the client completes, so a running
// transfer shows up in the interval where it happens. It is accounting of bytes written to
// Steam's connections, not an independent speed test.
func intervalMbps(previous, current uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 || current < previous {
		return 0
	}
	return float64(current-previous) * 8 / elapsed.Seconds() / 1e6
}

// concurrencyTracker turns cumulative per-path byte counters into interval facts. An interval is
// concurrent only when both paths exceed concurrentFloorMbps within that same interval, and active
// when at least one does. Bytes that one path moved earlier and the other later never make
// concurrency: the cumulative counters only feed bothEver. Not safe for concurrent use.
type concurrencyTracker struct {
	last                       time.Time
	lastHome, lastPhone        uint64
	home, phone                uint64 // cumulative bytes at the last observation
	activeBytesHome            uint64 // bytes moved during active intervals only
	activeBytesPhone           uint64
	active, concurrent, streak time.Duration
	longest                    time.Duration
	bothNow                    bool
}

func newConcurrencyTracker(start time.Time) *concurrencyTracker {
	return &concurrencyTracker{last: start}
}

// observe records the cumulative counters at now and returns the interval rates. A call whose
// time does not advance changes nothing, so the bytes stay for the next interval.
func (c *concurrencyTracker) observe(now time.Time, home, phone uint64) (homeMbps, phoneMbps float64) {
	elapsed := now.Sub(c.last)
	if elapsed <= 0 {
		return 0, 0
	}
	homeMbps = intervalMbps(c.lastHome, home, elapsed)
	phoneMbps = intervalMbps(c.lastPhone, phone, elapsed)
	homeUp, phoneUp := homeMbps > concurrentFloorMbps, phoneMbps > concurrentFloorMbps
	if homeUp || phoneUp {
		c.active += elapsed
		c.activeBytesHome += home - c.lastHome
		c.activeBytesPhone += phone - c.lastPhone
	}
	c.bothNow = homeUp && phoneUp
	if c.bothNow {
		c.concurrent += elapsed
		c.streak += elapsed
		if c.streak > c.longest {
			c.longest = c.streak
		}
	} else {
		c.streak = 0
	}
	c.last, c.lastHome, c.lastPhone, c.home, c.phone = now, home, phone, home, phone
	return homeMbps, phoneMbps
}

type sustainedMbps struct {
	Home     float64 `json:"home"`
	Phone    float64 `json:"phone"`
	Combined float64 `json:"combined"`
}

func (c *concurrencyTracker) sustained() sustainedMbps {
	if c.active <= 0 {
		return sustainedMbps{}
	}
	rate := func(b uint64) float64 { return float64(b) * 8 / c.active.Seconds() / 1e6 }
	return sustainedMbps{rate(c.activeBytesHome), rate(c.activeBytesPhone), rate(c.activeBytesHome + c.activeBytesPhone)}
}

// phoneAuthReport is the phone token's state. It has no field that can hold the token.
type phoneAuthReport struct {
	Stale      bool       `json:"stale"`
	Generation uint64     `json:"generation"`
	Rejections uint64     `json:"rejections"`
	StaleSince *time.Time `json:"stale_since,omitempty"`
}

// statusReport is the JSON diagnostic summary. Its fields are listed in docs/STEAM-RELIABILITY-PLAN.md
// (R11). It never carries a token or a download URL and makes no claim about Steam adoption.
type statusReport struct {
	Started        time.Time        `json:"started"`
	Experimental   bool             `json:"experimental"`
	SteamDiscovery bool             `json:"steam_discovery_configured"`
	UplinkMode     string           `json:"uplink_mode"`
	Uplinks        any              `json:"uplinks"`
	HomeMbps       float64          `json:"home_interval_mbps"`
	PhoneMbps      float64          `json:"phone_interval_mbps"`
	BothActiveNow  bool             `json:"both_paths_active_now"`
	ConcurrentSecs float64          `json:"concurrent_seconds"`
	LongestSecs    float64          `json:"longest_concurrent_seconds"`
	BothEver       bool             `json:"both_paths_ever"`
	ActiveSecs     float64          `json:"active_seconds"`
	SustainedMbps  sustainedMbps    `json:"sustained_mbps"`
	PhoneAuth      *phoneAuthReport `json:"phone_auth"`
}

func newStatusReport(started time.Time, discovery bool, mode steamaccel.UplinkMode, status steamaccel.Status, homeMbps, phoneMbps float64, c *concurrencyTracker) statusReport {
	r := statusReport{
		Started: started, Experimental: true, SteamDiscovery: discovery,
		UplinkMode: modeName(mode), Uplinks: status,
		HomeMbps: homeMbps, PhoneMbps: phoneMbps,
		BothActiveNow:  c.bothNow,
		ConcurrentSecs: c.concurrent.Seconds(), LongestSecs: c.longest.Seconds(),
		BothEver:   status.Home.Bytes > 0 && status.Phone.Bytes > 0,
		ActiveSecs: c.active.Seconds(), SustainedMbps: c.sustained(),
	}
	if mode != steamaccel.UplinkHomeOnly { // home mode has no phone token
		auth := &phoneAuthReport{Stale: status.PhoneAuth.Stale, Generation: status.PhoneAuth.Generation, Rejections: status.PhoneAuth.Rejections}
		if !status.PhoneAuth.StaleSince.IsZero() {
			since := status.PhoneAuth.StaleSince
			auth.StaleSince = &since
		}
		r.PhoneAuth = auth
	}
	return r
}

// modeName is the --uplink-mode spelling of a mode.
func modeName(m steamaccel.UplinkMode) string {
	switch m {
	case steamaccel.UplinkHomeOnly:
		return "home"
	case steamaccel.UplinkPhoneOnly:
		return "phone"
	}
	return "dual"
}

func parseUplinkMode(value string) (steamaccel.UplinkMode, error) {
	switch value {
	case "dual":
		return steamaccel.UplinkDual, nil
	case "home":
		return steamaccel.UplinkHomeOnly, nil
	case "phone":
		return steamaccel.UplinkPhoneOnly, nil
	}
	return 0, fmt.Errorf("--uplink-mode must be dual, home or phone")
}

// defaultReportPath keeps each mode's report apart so baselines do not overwrite each other.
func defaultReportPath(m steamaccel.UplinkMode) string {
	return "tetherflow-status-" + modeName(m) + ".json"
}

// writeReport replaces path atomically: a reader sees the old or the new report, never a torn one.
func writeReport(path string, r statusReport) error {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(append(encoded, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
