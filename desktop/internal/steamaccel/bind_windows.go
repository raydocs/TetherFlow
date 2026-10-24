//go:build windows

package steamaccel

import (
	"encoding/binary"
	"errors"
	"net"
	"syscall"
)

// IP_UNICAST_IF prevents a new phone default route from stealing home sockets.
// Windows requires the IPv4 interface index in network byte order.
func configureHomeDialer(d *net.Dialer, home net.IP) error {
	interfaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	index := 0
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.Equal(home) {
				index = iface.Index
				break
			}
		}
		if index != 0 {
			break
		}
	}
	if index == 0 {
		return errors.New("HomeIPv4 is not assigned to a local Windows interface")
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(index))
	// Winsock setsockopt copies the integer in native (little-endian) byte order.
	value := int(binary.LittleEndian.Uint32(b[:]))
	d.Control = func(_, _ string, raw syscall.RawConn) error {
		var sockErr error
		err := raw.Control(func(fd uintptr) { sockErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 31, value) })
		if err != nil {
			return err
		}
		return sockErr
	}
	return nil
}
