package usbmux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testDevice = Device{DeviceID: 7, Serial: "abcdef", ConnectionType: "USB", ConnectionSpeed: 480000000}

func deviceXML(id uint32, serial, kind string) string {
	return fmt.Sprintf(`<dict><key>DeviceID</key><integer>%d</integer><key>Properties</key><dict><key>DeviceID</key><integer>%d</integer><key>SerialNumber</key><string>%s</string><key>ConnectionType</key><string>%s</string><key>ConnectionSpeed</key><integer>480000000</integer></dict></dict>`, id, id, serial, kind)
}
func listXML(entries ...string) string {
	return plist(`<dict><key>DeviceList</key><array>` + strings.Join(entries, "") + `</array></dict>`)
}
func resultXML(n uint64) string {
	return plist(fmt.Sprintf(`<dict><key>MessageType</key><string>Result</string><key>Number</key><integer>%d</integer></dict>`, n))
}
func packet(body string, tag uint32) []byte {
	p := make([]byte, 16+len(body))
	binary.LittleEndian.PutUint32(p, uint32(len(p)))
	binary.LittleEndian.PutUint32(p[4:], 1)
	binary.LittleEndian.PutUint32(p[8:], 8)
	binary.LittleEndian.PutUint32(p[12:], tag)
	copy(p[16:], body)
	return p
}
func readRequest(c net.Conn) (map[string]any, uint32, error) {
	var h [16]byte
	if _, e := io.ReadFull(c, h[:]); e != nil {
		return nil, 0, e
	}
	n := binary.LittleEndian.Uint32(h[:])
	if n < 16 || n > maxPacket || binary.LittleEndian.Uint32(h[4:]) != 1 || binary.LittleEndian.Uint32(h[8:]) != 8 {
		return nil, 0, fmt.Errorf("bad request header")
	}
	b := make([]byte, n-16)
	if _, e := io.ReadFull(c, b); e != nil {
		return nil, 0, e
	}
	m, e := parsePlist(b)
	return m, binary.LittleEndian.Uint32(h[12:]), e
}
func replyList(c net.Conn, entries ...string) error {
	m, tag, e := readRequest(c)
	if e != nil {
		return e
	}
	if m["MessageType"] != "ListDevices" {
		return fmt.Errorf("expected ListDevices, got %v", m)
	}
	return writeFull(c, packet(listXML(entries...), tag))
}
func expectConnect(c net.Conn) (uint32, error) {
	m, tag, e := readRequest(c)
	if e != nil {
		return 0, e
	}
	if m["MessageType"] != "Connect" || m["DeviceID"] != uint64(7) || m["PortNumber"] != uint64(0x5b20) {
		return 0, fmt.Errorf("wrong Connect: %v", m)
	}
	return tag, nil
}

type fakeMux struct {
	listener net.Listener
	mu       sync.Mutex
	active   map[net.Conn]bool
	wg       sync.WaitGroup
	errors   chan error
	stopped  bool
}

func startFake(t *testing.T, handler func(net.Conn) error) (*Client, *fakeMux) {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	f := &fakeMux{listener: l, active: map[net.Conn]bool{}, errors: make(chan error, 128)}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			f.mu.Lock()
			if f.stopped {
				f.mu.Unlock()
				c.Close()
				return
			}
			f.active[c] = true
			f.wg.Add(1)
			f.mu.Unlock()
			go func() {
				defer f.wg.Done()
				defer func() { c.Close(); f.mu.Lock(); delete(f.active, c); f.mu.Unlock() }()
				if e := handler(c); e != nil {
					f.errors <- e
				}
			}()
		}
	}()
	t.Cleanup(func() {
		f.mu.Lock()
		f.stopped = true
		f.listener.Close()
		for c := range f.active {
			c.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
	client, e := NewClient(Config{Network: "tcp", Address: l.Addr().String(), HandshakeTimeout: 2 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	return client, f
}
func (f *fakeMux) check(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		count := len(f.active)
		f.mu.Unlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake mux session did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case e := <-f.errors:
		t.Fatal(e)
	default:
	}
}
func boundedContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func TestListCanonicalFragmentationAndUSBSelection(t *testing.T) {
	client, f := startFake(t, func(c net.Conn) error {
		m, tag, e := readRequest(c)
		if e != nil {
			return e
		}
		if m["MessageType"] != "ListDevices" {
			return fmt.Errorf("not list")
		}
		p := packet(listXML(deviceXML(7, "AB-CD-EF", "USB"), deviceXML(8, "wifi", "Network")), tag)
		for _, b := range p {
			if e := writeFull(c, []byte{b}); e != nil {
				return e
			}
		}
		return nil
	})
	devices, e := client.ListUSB(boundedContext(t))
	if e != nil {
		t.Fatal(e)
	}
	if len(devices) != 1 || devices[0] != testDevice {
		t.Fatalf("devices=%+v", devices)
	}
	d, e := SelectUSB(devices, 7, " ABC-DEF ")
	if e != nil || d != testDevice {
		t.Fatalf("selection %+v %v", d, e)
	}
	f.check(t)
}
func TestSelection(t *testing.T) {
	other := testDevice
	other.DeviceID = 8
	other.Serial = "other"
	for _, tc := range []struct {
		id     uint32
		serial string
		want   error
	}{{0, "", ErrSelectionRequired}, {7, "other", ErrDeviceNotFound}, {8, "", nil}, {0, "other", nil}} {
		_, e := SelectUSB([]Device{testDevice, other}, tc.id, tc.serial)
		if !errors.Is(e, tc.want) {
			t.Fatalf("%+v got %v", tc, e)
		}
	}
	if _, e := SelectUSB([]Device{{DeviceID: 7, Serial: "x", ConnectionType: "Network"}}, 0, ""); !errors.Is(e, ErrDeviceNotFound) {
		t.Fatal(e)
	}
}
func TestEndpointRestrictions(t *testing.T) {
	for _, cfg := range []Config{{Network: "tcp", Address: "192.0.2.1:27015"}, {Network: "tcp", Address: "localhost:27015"}, {Network: "tcp", Address: "0.0.0.0:27015"}, {Network: "tcp", Address: "127.0.0.1:0"}, {Network: "tcp", Address: "[::1%lo0]:27015"}, {Network: "unix", Address: "relative"}, {Network: "udp", Address: "127.0.0.1:27015"}, {HandshakeTimeout: -1}} {
		if _, e := NewClient(cfg); e == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	for _, cfg := range []Config{{}, {Network: "tcp", Address: "[::1]:27015"}, {Network: "tcp", Address: "127.0.0.2:27015"}} {
		if _, e := NewClient(cfg); e != nil {
			t.Fatal(e)
		}
	}
}
func TestProtocolHeaderFailures(t *testing.T) {
	for _, name := range []string{"oversize", "short", "version", "message", "tag", "truncated header", "truncated body"} {
		t.Run(name, func(t *testing.T) {
			client, _ := startFake(t, func(c net.Conn) error {
				_, tag, e := readRequest(c)
				if e != nil {
					return e
				}
				p := packet(listXML(), tag)
				switch name {
				case "oversize":
					binary.LittleEndian.PutUint32(p, maxPacket+1)
					p = p[:16]
				case "short":
					binary.LittleEndian.PutUint32(p, 15)
					p = p[:16]
				case "version":
					binary.LittleEndian.PutUint32(p[4:], 0)
				case "message":
					binary.LittleEndian.PutUint32(p[8:], 1)
				case "tag":
					binary.LittleEndian.PutUint32(p[12:], tag+1)
				case "truncated header":
					p = p[:8]
				case "truncated body":
					p = p[:len(p)-1]
				}
				return writeFull(c, p)
			})
			_, e := client.ListUSB(boundedContext(t))
			if e == nil {
				t.Fatal("accepted malformed packet")
			}
			if !strings.HasPrefix(name, "truncated") && !errors.Is(e, ErrProtocol) {
				t.Fatalf("got %v", e)
			}
		})
	}
}
func TestDeviceValidation(t *testing.T) {
	good := deviceXML(7, "AB-CD-EF", "USB")
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"ID mismatch", listXML(strings.Replace(good, `<key>DeviceID</key><integer>7</integer>`, `<key>DeviceID</key><integer>8</integer>`, 1)), ErrProtocol},
		{"duplicate ID", listXML(good, good), ErrProtocol},
		{"serial aliases", listXML(strings.Replace(good, `<key>ConnectionType</key>`, `<key>UDID</key><string>different</string><key>ConnectionType</key>`, 1)), ErrProtocol},
		{"no serial", listXML(strings.Replace(good, `<key>SerialNumber</key><string>AB-CD-EF</string>`, "", 1)), ErrProtocol},
		{"bad integer type", listXML(strings.ReplaceAll(good, `<integer>7</integer>`, `<string>7</string>`)), ErrProtocol},
		{"malformed plist", plist(`<dict><key>DeviceList</key><dict/></dict>`), ErrProtocol},
		{"result error", resultXML(3), &ResultError{Number: 3}},
		{"result no number", plist(`<dict><key>MessageType</key><string>Result</string></dict>`), ErrProtocol},
		{"wrong message", plist(`<dict><key>MessageType</key><string>Attached</string></dict>`), ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := startFake(t, func(c net.Conn) error {
				_, tag, e := readRequest(c)
				if e != nil {
					return e
				}
				return writeFull(c, packet(tc.body, tag))
			})
			_, e := client.ListUSB(boundedContext(t))
			var result *ResultError
			if errors.As(tc.want, &result) {
				var got *ResultError
				if !errors.As(e, &got) || got.Number != result.Number {
					t.Fatal(e)
				}
			} else if !errors.Is(e, tc.want) {
				t.Fatal(e)
			}
		})
	}
}
func TestConnectExactDeviceAndRetention(t *testing.T) {
	payload := bytes.Repeat([]byte("final-bytes"), 8192)
	client, f := startFake(t, func(c net.Conn) error {
		if e := replyList(c, deviceXML(7, "AB-CD-EF", "USB")); e != nil {
			return e
		}
		tag, e := expectConnect(c)
		if e != nil {
			return e
		}
		return writeFull(c, append(packet(resultXML(0), tag), payload...))
	})
	conn, e := client.Connect(boundedContext(t), testDevice, 8283)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	got, e := io.ReadAll(conn)
	if e != nil || !bytes.Equal(got, payload) {
		t.Fatalf("bytes=%d error=%v", len(got), e)
	}
	f.check(t)
}
func TestConnectDoesNotFallback(t *testing.T) {
	for _, entry := range []string{deviceXML(8, "ABCDEF", "USB"), deviceXML(7, "other", "USB"), deviceXML(7, "ABCDEF", "Network"), ""} {
		t.Run(entry, func(t *testing.T) {
			client, f := startFake(t, func(c net.Conn) error {
				if e := replyList(c, entry); e != nil {
					return e
				}
				var b [1]byte
				n, _ := c.Read(b[:])
				if n != 0 {
					return fmt.Errorf("sent Connect for wrong device")
				}
				return nil
			})
			_, e := client.Connect(boundedContext(t), testDevice, 8283)
			if !errors.Is(e, ErrDeviceNotFound) {
				t.Fatal(e)
			}
			f.check(t)
		})
	}
}
func TestConnectResultValidation(t *testing.T) {
	for _, body := range []string{resultXML(2), plist(`<dict/>`), plist(`<dict><key>MessageType</key><string>Result</string><key>Number</key><string>0</string></dict>`)} {
		client, _ := startFake(t, func(c net.Conn) error {
			if e := replyList(c, deviceXML(7, "abcdef", "USB")); e != nil {
				return e
			}
			tag, e := expectConnect(c)
			if e != nil {
				return e
			}
			return writeFull(c, packet(body, tag))
		})
		if _, e := client.Connect(boundedContext(t), testDevice, 8283); e == nil {
			t.Fatal("accepted bad Result")
		}
	}
}
func TestHandshakeCancelAndDeadline(t *testing.T) {
	for _, stage := range []string{"dial", "list", "connect"} {
		t.Run(stage, func(t *testing.T) {
			ready := make(chan struct{})
			var client *Client
			var e error
			if stage == "dial" {
				client, e = NewClient(Config{Network: "tcp", Address: "127.0.0.1:27015", DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					close(ready)
					<-ctx.Done()
					return nil, ctx.Err()
				}})
				if e != nil {
					t.Fatal(e)
				}
			} else {
				client, _ = startFake(t, func(c net.Conn) error {
					if stage == "connect" {
						if e := replyList(c, deviceXML(7, "abcdef", "USB")); e != nil {
							return e
						}
						if _, e := expectConnect(c); e != nil {
							return e
						}
					} else {
						if _, _, e := readRequest(c); e != nil {
							return e
						}
					}
					close(ready)
					var b [1]byte
					_, _ = c.Read(b[:])
					return nil
				})
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { _, e := client.Connect(ctx, testDevice, 8283); done <- e }()
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("not ready")
			}
			cancel()
			select {
			case e := <-done:
				if !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("cancel did not interrupt handshake")
			}
		})
	}
	client, _ := startFake(t, func(c net.Conn) error { _, _, _ = readRequest(c); var b [1]byte; _, _ = c.Read(b[:]); return nil })
	client.config.HandshakeTimeout = 30 * time.Millisecond
	_, e := client.ListUSB(context.Background())
	if e == nil {
		t.Fatal("missing deadline")
	}
}
func TestConnectClearsDeadlineAndDetachesContext(t *testing.T) {
	release := make(chan struct{})
	client, f := startFake(t, func(c net.Conn) error {
		if e := replyList(c, deviceXML(7, "abcdef", "USB")); e != nil {
			return e
		}
		tag, e := expectConnect(c)
		if e != nil {
			return e
		}
		if e = writeFull(c, packet(resultXML(0), tag)); e != nil {
			return e
		}
		select {
		case <-release:
		case <-time.After(time.Second):
			return errors.New("test release not signaled")
		}
		return writeFull(c, []byte("still alive"))
	})
	client.config.HandshakeTimeout = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	conn, e := client.Connect(ctx, testDevice, 8283)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	cancel()
	time.Sleep(65 * time.Millisecond)
	close(release)
	got, e := io.ReadAll(conn)
	if e != nil || string(got) != "still alive" {
		t.Fatalf("%q %v", got, e)
	}
	f.check(t)
}
func TestClientConcurrent(t *testing.T) {
	client, f := startFake(t, func(c net.Conn) error { return replyList(c, deviceXML(7, "abcdef", "USB")) })
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := client.ListUSB(boundedContext(t)); e != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatal("concurrent failure")
	}
	f.check(t)
}
