//go:build darwin

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"

	"tetherflow-desktop/internal/usbmux"
)

// A local protocol service fixture, not an Apple service or USB driver. Actual
// Client.ListUSB, Client.Connect and Forwarder are used unchanged end-to-end.
func nativeMuxTunnel(t *testing.T, phone string) string {
	t.Helper()
	service, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	active := map[net.Conn]bool{}
	closed := false
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = service.Close()
			mu.Lock()
			closed = true
			for c := range active {
				_ = c.Close()
			}
			mu.Unlock()
			wg.Wait()
		})
	}
	t.Cleanup(stop)
	go func() {
		for {
			conn, e := service.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			active[conn] = true
			wg.Add(1)
			mu.Unlock()
			go func() {
				defer wg.Done()
				defer func() { _ = conn.Close(); mu.Lock(); delete(active, conn); mu.Unlock() }()
				serveMuxFixture(t, conn, phone)
			}()
		}
	}()
	client, e := usbmux.NewClient(usbmux.Config{Network: "tcp", Address: service.Addr().String()})
	if e != nil {
		t.Fatal(e)
	}
	devices, e := client.ListUSB(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	device, e := usbmux.SelectUSB(devices, 0, "")
	if e != nil {
		t.Fatal(e)
	}
	fwd, e := usbmux.New(client, device)
	if e != nil {
		t.Fatal(e)
	}
	local, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fwd.Serve(lifetime, local) }()
	t.Cleanup(func() { cancel(); _ = fwd.Close(); <-done })
	return local.Addr().String()
}
func serveMuxFixture(t *testing.T, conn net.Conn, phone string) {
	for {
		var header [16]byte
		if _, e := io.ReadFull(conn, header[:]); e != nil {
			return
		}
		size := binary.LittleEndian.Uint32(header[:4])
		if size < 16 || size > 1<<20 {
			t.Error("invalid native client frame")
			return
		}
		body := make([]byte, int(size)-16)
		if _, e := io.ReadFull(conn, body); e != nil {
			return
		}
		values := map[string]string{}
		decoder := xml.NewDecoder(bytes.NewReader(body))
		key := ""
		for {
			token, e := decoder.Token()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Error(e)
				return
			}
			element, ok := token.(xml.StartElement)
			if !ok {
				continue
			}
			if element.Name.Local == "key" {
				if e = decoder.DecodeElement(&key, &element); e != nil {
					t.Error(e)
					return
				}
			} else if element.Name.Local == "string" || element.Name.Local == "integer" {
				var value string
				if e = decoder.DecodeElement(&value, &element); e != nil {
					t.Error(e)
					return
				}
				values[key] = value
			}
		}
		reply := func(dict string) bool {
			payload := []byte(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict>` + dict + `</dict></plist>`)
			binary.LittleEndian.PutUint32(header[:4], uint32(16+len(payload)))
			_, e := io.Copy(conn, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(payload)))
			return e == nil
		}
		switch values["MessageType"] {
		case "ListDevices":
			if !reply(`<key>DeviceList</key><array><dict><key>DeviceID</key><integer>1</integer><key>Properties</key><dict><key>DeviceID</key><integer>1</integer><key>ConnectionType</key><string>USB</string><key>SerialNumber</key><string>fixture-usb</string><key>ConnectionSpeed</key><integer>10000000000</integer></dict></dict></array>`) {
				return
			}
		case "Connect":
			wanted := (usbmux.ForwardPort&0xff)<<8 | usbmux.ForwardPort>>8
			if values["DeviceID"] != "1" || values["PortNumber"] != strconv.Itoa(int(wanted)) {
				t.Errorf("wrong USB device/port: %v", values)
				return
			}
			upstream, e := net.Dial("tcp", phone)
			if e != nil {
				_ = reply(`<key>MessageType</key><string>Result</string><key>Number</key><integer>3</integer>`)
				return
			}
			defer upstream.Close()
			if !reply(`<key>MessageType</key><string>Result</string><key>Number</key><integer>0</integer>`) {
				return
			}
			done := make(chan struct{}, 1)
			go func() {
				_, _ = io.Copy(upstream, conn)
				if c, ok := upstream.(interface{ CloseWrite() error }); ok {
					_ = c.CloseWrite()
				}
				done <- struct{}{}
			}()
			_, _ = io.Copy(conn, upstream)
			if c, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = c.CloseWrite()
			}
			<-done
			return
		default:
			t.Error(fmt.Errorf("unexpected USB message %q", values["MessageType"]))
			return
		}
	}
}
