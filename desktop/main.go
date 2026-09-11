package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultPort = 8282
)

var candidateIPs = []string{
	"192.168.42.129", // Android USB Tethering (RNDIS / CDC-NCM default)
	"192.168.49.1",   // Android Wi-Fi Direct default
	"192.168.43.1",   // Android LocalHotspot default
	"127.0.0.1",      // ADB Forward / USB Tunnel (Instant test & zero-configuration)
}

func main() {
	fmt.Println("=====================================================")
	fmt.Println("       TetherFlow Desktop Companion (Win & Mac)      ")
	fmt.Println("=====================================================")
	fmt.Println("[Status] Zero-touch background engine started.")
	fmt.Println("[Status] Waiting for TetherFlow phone (USB or Wi-Fi)...")

	// Ensure system proxy is cleaned up on exit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\n[Exiting] Cleaning up system proxy settings...")
		DisableSystemProxy()
		os.Exit(0)
	}()

	client := &http.Client{
		Timeout: 600 * time.Millisecond,
	}

	isConnected := false
	var activeIP string
	missCount := 0

	for {
		if !isConnected {
			foundIP := probeTetherFlow(client)
			if foundIP != "" {
				fmt.Printf("\n[+] TetherFlow detected at %s:%d!\n", foundIP, defaultPort)
				fmt.Println("[+] Enabling zero-loss 5G tunnel on this computer...")
				if err := EnableSystemProxy(foundIP, defaultPort); err != nil {
					fmt.Printf("[!] Failed to set system proxy: %v\n", err)
				} else {
					fmt.Println("[SUCCESS] 🚀 Connected! Computer is now using phone 5G directly.")
					fmt.Println("[INFO] Hotspot quota bypassed. Zero configuration needed.")
					NotifyUser("TetherFlow 🚀", "已接入三星 5G 满速网络！(零热点配额消耗)")
					isConnected = true
					activeIP = foundIP
					missCount = 0
				}
			}
		} else {
			// Check if active phone is still alive
			if checkAlive(client, activeIP, defaultPort) {
				missCount = 0
			} else {
				missCount++
				if missCount >= 2 {
					fmt.Printf("\n[-] Phone disconnected (%s).\n", activeIP)
					fmt.Println("[-] Restoring standard computer network...")
					DisableSystemProxy()
					NotifyUser("TetherFlow", "手机已断开，已自动恢复默认网络。")
					isConnected = false
					activeIP = ""
					missCount = 0
					fmt.Println("[Status] Waiting for TetherFlow phone...")
				}
			}
		}
		time.Sleep(1500 * time.Millisecond)
	}
}

func probeTetherFlow(client *http.Client) string {
	for _, ip := range candidateIPs {
		if checkAlive(client, ip, defaultPort) {
			return ip
		}
	}
	return ""
}

func checkAlive(client *http.Client, ip string, port int) bool {
	// Quick TCP port check first
	target := fmt.Sprintf("%s:%d", ip, port)
	conn, err := net.DialTimeout("tcp", target, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()

	// Verify HTTP probe
	url := fmt.Sprintf("http://%s/pac", target)
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}
