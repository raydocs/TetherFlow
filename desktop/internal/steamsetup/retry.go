package steamsetup

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RetryPolicy bounds UpdateWithRetry. Delays start at Initial (default 50ms), double up to
// Max (default 1s), and retrying stops once Total has elapsed. A zero Total means one attempt.
type RetryPolicy struct{ Initial, Max, Total time.Duration }

// retryObserve is a test seam, nil in production; it sees each attempt number.
var retryObserve func(attempt int)

// IsTransient reports whether err is a temporary sharing or lock failure worth retrying.
// Rewrite refusals and manual-recovery errors are never transient.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, ErrNeedsManualRecovery) {
		return false
	}
	return transientOS(err)
}

// UpdateWithRetry calls Update, retrying only transient errors until the budget or ctx ends.
func UpdateWithRetry(ctx context.Context, path, ip, lease string, p RetryPolicy) (bool, error) {
	if p.Initial <= 0 {
		p.Initial = 50 * time.Millisecond
	}
	if p.Max <= 0 {
		p.Max = time.Second
	}
	deadline := time.Now().Add(p.Total)
	delay := p.Initial
	for attempt := 1; ; attempt++ {
		if retryObserve != nil {
			retryObserve(attempt)
		}
		changed, err := Update(path, ip, lease)
		if err == nil || !IsTransient(err) {
			return changed, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, fmt.Errorf("hosts update still failing after %v: %w", p.Total, err)
		}
		timer := time.NewTimer(min(delay, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, fmt.Errorf("hosts update cancelled: %w (last error: %v)", ctx.Err(), err)
		case <-timer.C:
		}
		delay = min(delay*2, p.Max)
	}
}
