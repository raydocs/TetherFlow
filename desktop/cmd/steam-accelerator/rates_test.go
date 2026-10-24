package main

import (
	"testing"
	"time"
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
