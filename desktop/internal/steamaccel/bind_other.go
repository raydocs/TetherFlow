//go:build !windows

package steamaccel

import "net"

func configureHomeDialer(_ *net.Dialer, _ net.IP) error { return nil }
