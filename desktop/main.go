package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultPort = 8282
)

func main() {
	fmt.Println("=====================================================")
	fmt.Println("       TetherFlow Desktop Companion (Win & Mac)      ")
	fmt.Println("=====================================================")
	fmt.Println("[Status] Zero-touch background engine started.")
	fmt.Println("[Status] Auto-detecting TetherFlow via USB 3.0 or 5G Hotspot...")

	cleanup := func() {
		fmt.Println("\n[Exiting] Cleaning up system proxy settings...")
		DisableSystemProxy()
	}

	RegisterExitHandler(func() {
		cleanup()
		os.Exit(0)
	})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cleanup()
		os.Exit(0)
	}()

	tray := StartTray(func() {
		cleanup()
		os.Exit(0)
	})

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
				linkType, _, isWarn := DetectLinkDetails(foundIP)

				fmt.Printf("\n[+] 检测到 TetherFlow 节点: %s:%d [%s]\n", foundIP, defaultPort, linkType)
				fmt.Println("[+] 正在激活系统级零热点 5G 极速通道...")
				if err := EnableSystemProxy(foundIP, defaultPort); err != nil {
					fmt.Printf("[!] 设置系统代理失败: %v\n", err)
				} else {
					fmt.Println("[SUCCESS] 🚀 已连接！电脑现已直通手机原生 5G 网络。")
					fmt.Println("[INFO] 零热点配额扣除 (TTL=64 穿透，直连 nrphone 蜂窝 APN)。")
					
					tray.SetStatus(linkType, foundIP)
					balloonTitle := "TetherFlow 🚀"
					balloonMsg := fmt.Sprintf("已直连 5G 满速通道！\n协议: %s\n(零热点配额消耗)", linkType)
					if isWarn {
						balloonTitle = "TetherFlow ⚠️ (降级提醒)"
						balloonMsg = "已接入网络，但当前数据线处于 USB 2.0 模式 (上限 250Mbps)。\n如需 1,200Mbps 极速，请换插 USB 3.0 接口或粗线！"
					}
					tray.NotifyBalloon(balloonTitle, balloonMsg, isWarn)
					NotifyUser(balloonTitle, balloonMsg)

					isConnected = true
					activeIP = foundIP
					missCount = 0
				}
			}
		} else {
			// If connected wirelessly, periodically check if USB tunnel (127.0.0.1) is plugged in
			if activeIP != "127.0.0.1" {
				tryAdbForward()
				if checkAlive(client, "127.0.0.1", defaultPort) {
					linkType, _, isWarn := DetectLinkDetails("127.0.0.1")
					fmt.Printf("\n[🚀 发现 USB 极速连接] 自动平滑切换至 %s (127.0.0.1)...\n", linkType)
					EnableSystemProxy("127.0.0.1", defaultPort)
					activeIP = "127.0.0.1"
					missCount = 0
					tray.SetStatus(linkType, "127.0.0.1")
					tray.NotifyBalloon("TetherFlow 🚀", "已平滑切换至 USB 直连通道！\n"+linkType, isWarn)
				}
			}

			// Check if active phone is still alive
			if checkAlive(client, activeIP, defaultPort) {
				missCount = 0
			} else {
				missCount++
				if missCount >= 2 {
					fmt.Printf("\n[-] 手机已断开 (%s)。\n", activeIP)
					fmt.Println("[-] 正在恢复电脑默认网络...")
					DisableSystemProxy()
					tray.SetStatus("等待手机连接...", "")
					tray.NotifyBalloon("TetherFlow", "手机已断开，已自动恢复默认网络。", false)
					NotifyUser("TetherFlow", "手机已断开，已自动恢复默认网络。")
					isConnected = false
					activeIP = ""
					missCount = 0
					fmt.Println("[Status] 等待 TetherFlow 手机接入...")
				}
			}
		}
		time.Sleep(1500 * time.Millisecond)
	}
}

func getCandidateIPs() []string {
	var list []string
	seen := make(map[string]bool)

	add := func(ip string) {
		ip = strings.TrimSpace(ip)
		if ip != "" && !seen[ip] {
			seen[ip] = true
			list = append(list, ip)
		}
	}

	// 1. SuperSpeed USB ADB tunnel (Highest priority)
	add("127.0.0.1")

	// 2. Dynamic Default Gateways from OS routing table
	for _, gw := range GetSystemDefaultGateways() {
		add(gw)
	}

	// 3. Known Android gateway addresses
	add("192.168.43.1")   // Android LocalHotspot default
	add("192.168.49.1")   // Android Wi-Fi Direct default
	add("192.168.42.129") // Android USB Tethering (RNDIS / CDC-NCM default)

	// 4. Local interface subnet gateways
	for _, gw := range getInterfaceSubnetGateways() {
		add(gw)
	}

	return list
}

func getInterfaceSubnetGateways() []string {
	var gws []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				continue
			}
			ip := ipNet.IP.To4()
			if ip[0] == 169 && ip[1] == 254 {
				continue // skip link-local
			}
			gws = append(gws, fmt.Sprintf("%d.%d.%d.1", ip[0], ip[1], ip[2]))
			gws = append(gws, fmt.Sprintf("%d.%d.%d.254", ip[0], ip[1], ip[2]))
		}
	}
	return gws
}

func tryAdbForward() {
	var candidates []string
	if custom := os.Getenv("ADB_PATH"); custom != "" {
		candidates = append(candidates, custom)
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		candidates = append(candidates, filepath.Join(localAppData, "Android", "Sdk", "platform-tools", "adb.exe"))
	}
	if home := os.Getenv("USERPROFILE"); home != "" {
		candidates = append(candidates, filepath.Join(home, "platform-tools", "adb.exe"))
	}
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates, filepath.Join(home, "Library", "Android", "sdk", "platform-tools", "adb"))
	}
	candidates = append(candidates, "adb", "C:\\platform-tools\\adb.exe", "/usr/local/bin/adb", "/opt/homebrew/bin/adb")

	for _, p := range candidates {
		if _, err := exec.LookPath(p); err == nil || fileExists(p) {
			_ = exec.Command(p, "forward", "tcp:8282", "tcp:8282").Run()
			return
		}
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func probeTetherFlow(client *http.Client) string {
	tryAdbForward()
	candidates := getCandidateIPs()
	for _, ip := range candidates {
		if checkAlive(client, ip, defaultPort) {
			return ip
		}
	}
	return ""
}

func checkAlive(client *http.Client, ip string, port int) bool {
	// Quick TCP port check first
	target := net.JoinHostPort(ip, strconv.Itoa(port))
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

