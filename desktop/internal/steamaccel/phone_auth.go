package steamaccel

import (
	"errors"
	"strings"
	"time"
)

// PhoneAuthStatus reports the phone bearer token's state without ever exposing
// the token. Generation counts tokens installed (1 for Config.PhoneToken, +1 per
// accepted SetPhoneToken; 0 when no token was configured). Rejections counts
// CONNECT 407 answers received for the current generation. Stale is set by such a
// 407 and cleared only by SetPhoneToken; StaleSince is when it was first set.
type PhoneAuthStatus struct {
	Stale      bool
	Generation uint64
	Rejections uint64
	StaleSince time.Time
}

// errPhoneAuth means the phone refused (407) or would refuse the bearer token.
// It is a token-state event, never a path health event: see classification in
// ServeHTTP and serveTLS. It carries no token or response text.
var errPhoneAuth = errors.New("phone proxy authorization rejected")

var errPhonePathDisabled = errors.New("phone uplink is disabled in this mode")

// phoneAuth is guarded by Relay.mu, so the token and its generation are always
// read and written as one pair and Snapshot sees a consistent view.
type phoneAuth struct {
	token      string
	generation uint64
	rejections uint64
	stale      bool
	staleSince time.Time
}

const crockford32Alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// NormalizePhoneToken canonicalises a token typed or pasted by a person and
// validates it. Spaces, tabs, line breaks and hyphens (the display grouping) are
// removed and letters are lowercased. The result must be one of the formats the
// iOS app issues:
//
//   - 32 characters of lowercase Crockford base32 (0-9 a-z without i, l, o, u),
//     the current format, displayed in groups of 4;
//   - 64 lowercase hexadecimal characters, the legacy format.
//
// Crockford's decoding aliases (O to 0, I and L to 1) are not applied: the
// bearer string must equal what the phone holds. The error never echoes input.
func NormalizePhoneToken(raw string) (string, error) {
	if len(raw) > 4096 {
		return "", errors.New("phone token has an invalid format")
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '-':
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 'a' - 'A')
		default:
			b.WriteByte(c)
		}
	}
	token := b.String()
	switch len(token) {
	case 32:
		for i := 0; i < len(token); i++ {
			if strings.IndexByte(crockford32Alphabet, token[i]) < 0 {
				return "", errors.New("phone token has an invalid format")
			}
		}
	case 64:
		for i := 0; i < len(token); i++ {
			if c := token[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return "", errors.New("phone token has an invalid format")
			}
		}
	default:
		return "", errors.New("phone token has an invalid format")
	}
	return token, nil
}

// SetPhoneToken replaces the phone bearer token. The token is validated with
// NormalizePhoneToken first; an invalid one changes nothing. On success the
// token and a new generation are installed together, Stale is cleared, and idle
// phone sockets (tunnels opened with the old token) are closed. Active transfers
// are never interrupted. It does not probe the phone: a caller that must verify
// a token before swapping it does so before calling SetPhoneToken.
func (r *Relay) SetPhoneToken(token string) error {
	if !r.cfg.Uplinks.usesPhone() {
		return errPhonePathDisabled
	}
	n, err := NormalizePhoneToken(token)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return errors.New("relay closed")
	}
	r.auth.token = n
	r.auth.generation++
	r.auth.stale = false
	r.auth.staleSince = time.Time{}
	r.mu.Unlock()
	if t, ok := r.paths[1].transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
	return nil
}

// phoneCredentials returns the token with its generation, read under one lock.
func (r *Relay) phoneCredentials() (token string, generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auth.token, r.auth.generation
}

// markPhoneAuthStale records a 407 for the token generation that was sent. A 407
// for an older generation says nothing about the current token and is ignored.
func (r *Relay) markPhoneAuthStale(generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.auth.generation {
		return
	}
	r.auth.rejections++
	if !r.auth.stale {
		r.auth.stale = true
		r.auth.staleSince = time.Now()
	}
}

func (r *Relay) phoneStale() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auth.stale
}

func (a *phoneAuth) status() PhoneAuthStatus {
	return PhoneAuthStatus{Stale: a.stale, Generation: a.generation, Rejections: a.rejections, StaleSince: a.staleSince}
}
