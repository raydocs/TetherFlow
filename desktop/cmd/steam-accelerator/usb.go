package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"tetherflow-desktop/internal/usbmux"
)

func listPhones(ctx context.Context) error {
	client, err := usbmux.NewClient(usbmux.Config{})
	if err != nil {
		return err
	}
	devices, err := client.ListUSB(ctx)
	if err != nil {
		return fmt.Errorf("Apple USB service unavailable; install/open Apple Devices, connect and trust iPhone: %w", err)
	}
	if len(devices) == 0 {
		return fmt.Errorf("no USB iPhone found; Wi-Fi sync devices are deliberately excluded")
	}
	for _, device := range devices {
		fmt.Printf("USB device ID=%d, service-reported link=%.0f Mbps (not a throughput test)\n", device.DeviceID, float64(device.ConnectionSpeed)/1e6)
	}
	return nil
}

func startPhoneTunnel(ctx context.Context, serial string) (string, func(), error) {
	client, err := usbmux.NewClient(usbmux.Config{})
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
