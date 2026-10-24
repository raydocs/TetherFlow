// Package usbmux implements the local Apple USBmux plist protocol without a USB
// driver, subprocess, external dependency, or network-device fallback.
package usbmux

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	ErrProtocol          = errors.New("usbmux protocol error")
	ErrSelectionRequired = errors.New("multiple USB devices: select an exact device ID and/or serial")
	ErrDeviceNotFound    = errors.New("selected USB device not found")
	ErrClosed            = errors.New("usbmux forwarder closed")
)

type Config struct {
	Network          string
	Address          string
	HandshakeTimeout time.Duration
	DialContext      func(context.Context, string, string) (net.Conn, error)
}
type Client struct{ config Config }
type Device struct {
	DeviceID        uint32
	Serial          string
	ConnectionType  string
	ConnectionSpeed uint64
}
type ResultError struct{ Number uint64 }

func (e *ResultError) Error() string { return fmt.Sprintf("usbmux Result Number=%d", e.Number) }

func NewClient(c Config) (*Client, error) {
	if c.Network == "" {
		if runtime.GOOS == "windows" {
			c.Network = "tcp"
		} else {
			c.Network = "unix"
		}
	}
	if c.Address == "" {
		if c.Network == "unix" {
			c.Address = "/var/run/usbmuxd"
		} else {
			c.Address = "127.0.0.1:27015"
		}
	}
	switch c.Network {
	case "tcp", "tcp4", "tcp6":
		host, port, e := net.SplitHostPort(c.Address)
		ip := net.ParseIP(host)
		n, pe := strconv.ParseUint(port, 10, 16)
		if e != nil || ip == nil || !ip.IsLoopback() || pe != nil || n == 0 {
			return nil, fmt.Errorf("usbmux endpoint must be numeric loopback TCP with a nonzero port")
		}
	case "unix":
		if !filepath.IsAbs(c.Address) || strings.ContainsRune(c.Address, 0) {
			return nil, fmt.Errorf("usbmux Unix endpoint must be an absolute local path")
		}
	default:
		return nil, fmt.Errorf("unsupported usbmux service network %q", c.Network)
	}
	if c.HandshakeTimeout < 0 {
		return nil, fmt.Errorf("negative handshake timeout")
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 10 * time.Second
	}
	if c.DialContext == nil {
		d := &net.Dialer{}
		c.DialContext = d.DialContext
	}
	return &Client{config: c}, nil
}
func normalizeSerial(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}
func validDevice(d Device) bool {
	return d.DeviceID != 0 && d.ConnectionType == "USB" && normalizeSerial(d.Serial) != ""
}
func SelectUSB(devices []Device, id uint32, serial string) (Device, error) {
	serial = normalizeSerial(serial)
	var selected Device
	count := 0
	for _, d := range devices {
		if !validDevice(d) || (id != 0 && d.DeviceID != id) || (serial != "" && normalizeSerial(d.Serial) != serial) {
			continue
		}
		d.Serial = normalizeSerial(d.Serial)
		selected = d
		count++
	}
	if count == 0 {
		return Device{}, ErrDeviceNotFound
	}
	if count > 1 {
		return Device{}, ErrSelectionRequired
	}
	return selected, nil
}

type session struct {
	net.Conn
	reader *bufio.Reader
	tag    uint32
}

func (s *session) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *session) CloseWrite() error {
	if c, ok := s.Conn.(interface{ CloseWrite() error }); ok {
		return c.CloseWrite()
	}
	return errors.New("usbmux transport does not support half-close")
}

// A session reads exact packet lengths, leaving all prefetched raw bytes intact.
func (s *session) request(kind string, fields string) (map[string]any, error) {
	s.tag++
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<!" + appleDoctype + ">\n" + `<plist version="1.0"><dict><key>MessageType</key><string>` + kind + `</string><key>ClientVersionString</key><string>TetherFlow</string><key>ProgName</key><string>TetherFlow</string><key>kLibUSBMuxVersion</key><integer>3</integer>` + fields + `</dict></plist>`)
	var h [16]byte
	binary.LittleEndian.PutUint32(h[0:4], uint32(16+len(body)))
	binary.LittleEndian.PutUint32(h[4:8], 1)
	binary.LittleEndian.PutUint32(h[8:12], 8)
	binary.LittleEndian.PutUint32(h[12:16], s.tag)
	if e := writeFull(s.Conn, h[:]); e != nil {
		return nil, e
	}
	if e := writeFull(s.Conn, body); e != nil {
		return nil, e
	}
	if _, e := io.ReadFull(s.reader, h[:]); e != nil {
		return nil, e
	}
	length := binary.LittleEndian.Uint32(h[:4])
	if length < 16 || length > maxPacket {
		return nil, protocol("invalid packet length %d", length)
	}
	if binary.LittleEndian.Uint32(h[4:8]) != 1 || binary.LittleEndian.Uint32(h[8:12]) != 8 || binary.LittleEndian.Uint32(h[12:16]) != s.tag {
		return nil, protocol("response version/message/tag mismatch")
	}
	b := make([]byte, int(length)-16)
	if _, e := io.ReadFull(s.reader, b); e != nil {
		return nil, e
	}
	m, e := parsePlist(b)
	if e != nil {
		return nil, e
	}
	if typ, exists := m["MessageType"]; exists {
		t, ok := typ.(string)
		if !ok || t != "Result" {
			return nil, protocol("unexpected response MessageType")
		}
		n, ok := m["Number"].(uint64)
		if !ok {
			return nil, protocol("Result Number missing or invalid")
		}
		if n != 0 {
			return nil, &ResultError{Number: n}
		}
	}
	return m, nil
}
func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// stop must finish before clearing the handshake deadline: otherwise a racing
// cancellation callback could poison an otherwise successfully returned stream.
func (c *Client) open(ctx context.Context) (*session, func(bool) error, error) {
	if c == nil {
		return nil, nil, errors.New("nil usbmux client")
	}
	ctx, cancel := context.WithTimeout(ctx, c.config.HandshakeTimeout)
	conn, e := c.config.DialContext(ctx, c.config.Network, c.config.Address)
	if e != nil {
		cancel()
		return nil, nil, e
	}
	if conn == nil {
		cancel()
		return nil, nil, errors.New("dial returned nil connection")
	}
	deadline, _ := ctx.Deadline()
	if e = conn.SetDeadline(deadline); e != nil {
		conn.Close()
		cancel()
		return nil, nil, e
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { conn.Close(); close(done) })
	finish := func(keep bool) error {
		if !stop() {
			<-done
		}
		e := ctx.Err()
		cancel()
		if !keep || e != nil {
			conn.Close()
			return e
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			conn.Close()
			return err
		}
		return nil
	}
	return &session{Conn: conn, reader: bufio.NewReaderSize(conn, 32<<10)}, finish, nil
}
func (c *Client) ListUSB(ctx context.Context) (devices []Device, err error) {
	s, finish, e := c.open(ctx)
	if e != nil {
		return nil, e
	}
	defer func() {
		if e := finish(false); e != nil {
			err = e
		}
	}()
	return s.list()
}
func (s *session) list() ([]Device, error) {
	m, e := s.request("ListDevices", "")
	if e != nil {
		return nil, e
	}
	a, ok := m["DeviceList"].([]any)
	if !ok {
		return nil, protocol("DeviceList missing or invalid")
	}
	devices := make([]Device, 0, len(a))
	seen := map[uint32]bool{}
	for _, v := range a {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, protocol("invalid device entry")
		}
		p, ok := m["Properties"].(map[string]any)
		if !ok {
			return nil, protocol("device Properties missing")
		}
		outer, hasOuter := m["DeviceID"]
		inner, hasInner := p["DeviceID"]
		if !hasOuter && !hasInner {
			return nil, protocol("device ID missing")
		}
		var id uint64
		for _, entry := range []struct {
			v       any
			present bool
		}{{outer, hasOuter}, {inner, hasInner}} {
			if !entry.present {
				continue
			}
			n, ok := entry.v.(uint64)
			if !ok || n == 0 || n > 1<<32-1 || (id != 0 && id != n) {
				return nil, protocol("invalid/inconsistent device ID")
			}
			id = n
		}
		if seen[uint32(id)] {
			return nil, protocol("duplicate device ID")
		}
		seen[uint32(id)] = true
		typ, ok := p["ConnectionType"].(string)
		if !ok {
			return nil, protocol("ConnectionType missing")
		}
		if typ != "USB" {
			continue
		}
		var serial string
		for _, key := range []string{"SerialNumber", "UDID"} {
			if v, exists := p[key]; exists {
				s, ok := v.(string)
				if !ok || normalizeSerial(s) == "" {
					return nil, protocol("invalid device serial")
				}
				s = normalizeSerial(s)
				if serial != "" && serial != s {
					return nil, protocol("inconsistent serial/UDID")
				}
				serial = s
			}
		}
		if serial == "" {
			return nil, protocol("USB serial missing")
		}
		var speed uint64
		if v, exists := p["ConnectionSpeed"]; exists {
			var ok bool
			speed, ok = v.(uint64)
			if !ok {
				return nil, protocol("invalid ConnectionSpeed")
			}
		}
		devices = append(devices, Device{DeviceID: uint32(id), Serial: serial, ConnectionType: typ, ConnectionSpeed: speed})
	}
	return devices, nil
}
func (c *Client) Connect(ctx context.Context, device Device, port uint16) (conn net.Conn, err error) {
	if !validDevice(device) {
		return nil, ErrDeviceNotFound
	}
	if port == 0 {
		return nil, errors.New("usbmux port must be nonzero")
	}
	s, finish, e := c.open(ctx)
	if e != nil {
		return nil, e
	}
	keep := false
	defer func() {
		if e := finish(keep); e != nil {
			conn = nil
			err = e
		}
	}()
	devices, e := s.list()
	if e != nil {
		return nil, e
	}
	if _, e = SelectUSB(devices, device.DeviceID, device.Serial); e != nil {
		return nil, e
	}
	// plist PortNumber contains the 16-bit network-byte-order integer.
	networkPort := (port&255)<<8 | port>>8
	m, e := s.request("Connect", fmt.Sprintf("<key>DeviceID</key><integer>%d</integer><key>PortNumber</key><integer>%d</integer>", device.DeviceID, networkPort))
	if e != nil {
		return nil, e
	}
	if m["MessageType"] != "Result" {
		return nil, protocol("Connect requires Result")
	}
	keep = true
	return s, nil
}
