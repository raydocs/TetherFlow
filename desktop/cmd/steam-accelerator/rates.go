package main

import "time"

// intervalMbps is completed payload/wire accounting over the reporting interval,
// not an independent speed test. In-flight bytes can be attributed at completion.
func intervalMbps(previous, current uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 || current < previous {
		return 0
	}
	return float64(current-previous) * 8 / elapsed.Seconds() / 1e6
}
