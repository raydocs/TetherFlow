package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"time"

	"tetherflow-desktop/internal/usbmux"
)

// testUSBClient is nil in production; tests point the USB commands at a fake service.
var testUSBClient *usbmux.Client

func usbClient() (*usbmux.Client, error) {
	if testUSBClient != nil {
		return testUSBClient, nil
	}
	return usbmux.NewClient(usbmux.Config{})
}

// normalizeSerial is usbmux's selection normalization (lower case, dashes removed), so a
// printed serial is exactly what --device-udid matches. The round trip is under test.
func normalizeSerial(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}

// usbPhones keeps the USB entries startup could select, ordered by serial. Network
// (Wi-Fi sync) entries and entries without a serial or ID are not selectable.
func usbPhones(devices []usbmux.Device) []usbmux.Device {
	var out []usbmux.Device
	for _, d := range devices {
		if d.DeviceID != 0 && d.ConnectionType == "USB" && normalizeSerial(d.Serial) != "" {
			d.Serial = normalizeSerial(d.Serial)
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

func listUSBDevices(ctx context.Context) ([]usbmux.Device, error) {
	client, err := usbClient()
	if err != nil {
		return nil, err
	}
	return client.ListUSB(ctx)
}

// listPhones is the one place a selectable serial is printed: it only runs for the explicit
// --list-phones command. Doctor, logs and the report never carry it.
func listPhones(ctx context.Context, w io.Writer) error {
	devices, err := listUSBDevices(ctx)
	if err != nil {
		return fmt.Errorf("Apple USB service unavailable; install/open Apple Devices, connect and trust iPhone: %w", err)
	}
	phones := usbPhones(devices)
	if len(phones) == 0 {
		return fmt.Errorf("no USB iPhone found; Wi-Fi sync devices are deliberately excluded")
	}
	count := map[string]int{}
	for _, d := range phones {
		count[d.Serial]++
	}
	for _, d := range phones {
		line := fmt.Sprintf("serial=%s id=%d link=%dMbps", d.Serial, d.DeviceID, d.ConnectionSpeed/1_000_000)
		if count[d.Serial] > 1 {
			line += " duplicate-serial=selection-refused"
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "Pass a serial to --device-udid. link is the Apple USB service's metadata, not a throughput test.")
	return nil
}

func startPhoneTunnel(ctx context.Context, serial string) (string, func(), error) {
	client, err := usbClient()
	if err != nil {
		return "", nil, err
	}
	devices, err := client.ListUSB(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("Apple USB service not ready (no settings changed): %w", err)
	}
	device, err := usbmux.SelectUSB(devices, 0, serial)
	if err != nil {
		return "", nil, fmt.Errorf("select USB iPhone: %w; if multiple devices, specify --device-udid", err)
	}
	forwarder, err := usbmux.New(client, device)
	if err != nil {
		return "", nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	// The scan context is only a startup deadline. The owned tunnel has its own
	// lifetime, cancelled by cleanup rather than by expiration of that deadline.
	lifetime, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- forwarder.Serve(lifetime, listener) }()
	cleanup := func() {
		cancel()
		_ = forwarder.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("USB forwarder stopped: %v", err)
			}
		case <-time.After(5 * time.Second):
			log.Print("WARNING: USB forwarder teardown did not settle within5s")
		}
	}
	log.Printf("Built-in USB tunnel selected device ID=%d. Apple service reports %.0f Mbps link (NOT verified forwarding speed).", device.DeviceID, float64(device.ConnectionSpeed)/1e6)
	return listener.Addr().String(), cleanup, nil
}
