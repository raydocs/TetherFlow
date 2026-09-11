# TetherFlow 🌊

> **Next-Gen Zero-Loss No-Root Tethering & 5G Bypass Gateway**  
> Designed for Android 16 (Samsung One UI 8.5+ & modern Android devices) with zero-touch Desktop Companions for **Windows & macOS**.

[![Android](https://img.shields.io/badge/Android-16%2B%20(API%2036)-green.svg)](https://developer.android.com)
[![Platform](https://img.shields.io/badge/Desktop-Windows%20%7C%20macOS-blue.svg)](https://github.com/raydocs/TetherFlow/releases)
[![License](https://img.shields.io/badge/License-MIT-purple.svg)](LICENSE)

---

## 💡 Why TetherFlow? (为什么选择 TetherFlow)

Traditional carrier unlimited plans (like **AT&T Business Unlimited**, T-Mobile, Verizon) offer true unlimited unthrottled on-device data, but strictly cap or throttle hotspot/tethering data (e.g. 100GB cap, then 128kbps throttle).

Existing tools like **NetShare** are plagued with:
* ❌ Outdated Android 4.x UI, aggressive intrusive ads, and awkward setups.
* ❌ Heavy throughput degradation (500Mbps 5G drops to 40Mbps due to 4KB blocking buffers).
* ❌ Requiring users to manually type IP/Port proxies on computers every single time.

**TetherFlow solves this completely:**
* 🚀 **1:1 Native 5G Speed**: 64KB Direct Buffer Pool + Coroutine NIO Socket Pipeline. Zero throughput loss.
* 🛡️ **100% Hotspot Metering Bypass**: Routes userland packets through the primary cellular APN with TTL=64. Zero `dun` APN activation.
* ⚡ **Zero-Touch Auto-Connect (Win & Mac)**: With the ultra-lightweight desktop companion, you simply plug in the USB cable or connect to Wi-Fi. **The computer connects automatically without configuring any proxies!**
* 🎨 **Material 3 (Material You)**: Modern UI with real-time telemetry, bandwidth graphs, and dark mode.
* 📲 **One UI 8.5 Ready**: Includes a Samsung Quick Settings Tile and auto-start broadcast receiver on USB connect.

---

## 📦 Quick Download (快速下载)

Go to [**GitHub Releases**](https://github.com/raydocs/TetherFlow/releases/latest) to download:

| Component | File | Description |
| :--- | :--- | :--- |
| **Android App** | `TetherFlow-v1.0.0-debug.apk` | Install on Samsung / Android 14~16 phone |
| **Windows PC** | `tetherflow-win.exe` | 1-Click zero-touch companion for Windows 10/11 |
| **Apple Mac (M1/M2/M3/M4)** | `tetherflow-mac-arm64` | Native companion for Apple Silicon |
| **Intel Mac** | `tetherflow-mac-intel` | Native companion for Intel Macs |

---

## 🚀 How to Use (使用方法)

### 1. Android Phone Setup
1. Install and open **TetherFlow** on your phone.
2. Plug in the USB cable to your PC (or turn on Wi-Fi sharing).
3. On phone: Go to `Settings -> Connections -> Mobile Hotspot and Tethering -> Turn ON 'USB Tethering'`.
   *(TetherFlow will automatically detect and start the 5G engine in the background).*

### 2. Windows PC (100% Zero-Touch)
1. Download [**`tetherflow-win.exe`**](https://github.com/raydocs/TetherFlow/releases/latest).
2. Double-click to run.
3. Plug in your phone via USB cable -> **Your PC is instantly connected to high-speed 5G!**
   *(Unplugging the phone instantly restores your regular network).*

### 3. macOS (100% Zero-Touch)
1. Download [**`tetherflow-mac-arm64`**](https://github.com/raydocs/TetherFlow/releases/latest) (or intel version).
2. Run `./tetherflow-mac-arm64` in terminal or double-click.
3. Plug in phone or connect to phone's Wi-Fi -> **Done!**

---

## 🛠️ Architecture & Under the Hood

```
[ PC / Mac ]  <--- (USB RNDIS 0ms latency / 5GHz Wi-Fi) --->  [ Android Phone ]
     |                                                                |
[TetherFlow Desktop]                                       [TetherFlow Core Engine]
(Auto-detects gateway)                                     (HighSpeedProxyEngine)
     |                                                                |
  Auto-sets System Proxy                                   Binds socket to Cellular 5G
(Restores on disconnect)                                    (rmnet_data0 / Default APN)
                                                                      |
                                                          [ AT&T / Carrier Core ]
                                                          (TTL=64, Default APN, QCI 6)
                                                          *Zero Hotspot Metering*
```

---

## 📄 License
MIT License. Created with ❤️ for high-speed tethering freedom.
