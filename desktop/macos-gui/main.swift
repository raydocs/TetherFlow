import Cocoa
import Foundation

class AppDelegate: NSObject, NSApplicationDelegate {
    var statusItem: NSStatusItem!
    var timer: Timer?
    var isConnected = false
    var activeIP: String? = nil
    
    // Menu items
    let statusMenuItem = NSMenuItem(title: "🟡 等待手机连接...", action: nil, keyEquivalent: "")
    let ipMenuItem = NSMenuItem(title: "出口 IP: 检测中...", action: nil, keyEquivalent: "")
    let quotaMenuItem = NSMenuItem(title: "热点配额: 0 消耗 (已绕过)", action: nil, keyEquivalent: "")

    func applicationDidFinishLaunching(_ notification: Notification) {
        setupStatusItem()
        applyBrowserSpeedOptimizations()
        startDaemonAndMonitor()
    }

    func applyBrowserSpeedOptimizations() {
        let task = Process()
        task.launchPath = "/bin/sh"
        task.arguments = ["-c", "defaults write com.google.Chrome DisableQuic -bool true; defaults write com.microsoft.Edge DisableQuic -bool true; defaults write com.brave.Browser DisableQuic -bool true"]
        try? task.run()
    }
    
    func setupStatusItem() {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let button = statusItem.button {
            if #available(macOS 11.0, *) {
                button.image = NSImage(systemSymbolName: "bolt.fill", accessibilityDescription: "TetherFlow")
            } else {
                button.title = "⚡"
            }
        }
        
        let menu = NSMenu()
        menu.addItem(statusMenuItem)
        menu.addItem(ipMenuItem)
        menu.addItem(quotaMenuItem)
        menu.addItem(NSMenuItem.separator())
        menu.addItem(NSMenuItem(title: "🚀 打开 Speedtest 测速", action: #selector(openSpeedtest), keyEquivalent: "s"))
        menu.addItem(NSMenuItem(title: "🌐 打开 IP 检测 (ipinfo.io)", action: #selector(openIPInfo), keyEquivalent: "i"))
        menu.addItem(NSMenuItem.separator())
        menu.addItem(NSMenuItem(title: "退出 TetherFlow", action: #selector(quitApp), keyEquivalent: "q"))
        statusItem.menu = menu
    }
    
    func startDaemonAndMonitor() {
        checkConnection()
        timer = Timer.scheduledTimer(withTimeInterval: 2.5, repeats: true) { [weak self] _ in
            self?.checkConnection()
        }
    }
    
    func tryAdbForward() {
        let commonPaths = [
            "/Users/ruirui/Library/Android/sdk/platform-tools/adb",
            "/usr/local/bin/adb",
            "/opt/homebrew/bin/adb",
            "/opt/homebrew/share/android-commandlinetools/platform-tools/adb"
        ]
        for p in commonPaths {
            if FileManager.default.fileExists(atPath: p) {
                let task = Process()
                task.launchPath = p
                task.arguments = ["forward", "tcp:8282", "tcp:8282"]
                try? task.run()
                task.waitUntilExit()
                break
            }
        }
    }

    func getDefaultGateway() -> String? {
        let task = Process()
        task.launchPath = "/bin/sh"
        task.arguments = ["-c", "route -n get default | awk '/gateway/{print $2}'"]
        let pipe = Pipe()
        task.standardOutput = pipe
        try? task.run()
        task.waitUntilExit()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        let str = String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
        return (str != nil && !str!.isEmpty) ? str : nil
    }

    func getActiveNetworkServices() -> [String] {
        let task = Process()
        task.launchPath = "/usr/sbin/networksetup"
        task.arguments = ["-listallnetworkservices"]
        let pipe = Pipe()
        task.standardOutput = pipe
        try? task.run()
        task.waitUntilExit()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        guard let str = String(data: data, encoding: .utf8) else { return ["Wi-Fi"] }
        let lines = str.components(separatedBy: .newlines)
        var services: [String] = []
        for line in lines {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if !trimmed.isEmpty && !trimmed.contains("*") {
                services.append(trimmed)
            }
        }
        return services.isEmpty ? ["Wi-Fi"] : services
    }

    func checkConnection() {
        DispatchQueue.global(qos: .background).async { [weak self] in
            guard let self = self else { return }
            
            self.tryAdbForward()
            
            var candidates = ["127.0.0.1"]
            if let gw = self.getDefaultGateway() {
                candidates.append(gw)
            }
            candidates.append(contentsOf: ["192.168.43.1", "192.168.49.1", "192.168.42.129"])
            
            // Dedicated direct session to prevent proxy probe deadlock
            let probeConfig = URLSessionConfiguration.ephemeral
            probeConfig.connectionProxyDictionary = [:]
            probeConfig.timeoutIntervalForRequest = 0.5
            let probeSession = URLSession(configuration: probeConfig)
            
            var detectedIP: String? = nil

            for ip in candidates {
                guard let url = URL(string: "http://\(ip):8282/pac") else { continue }
                var req = URLRequest(url: url)
                req.timeoutInterval = 0.5
                
                let sem = DispatchSemaphore(value: 0)
                var ok = false
                let task = probeSession.dataTask(with: req) { _, resp, _ in
                    if let http = resp as? HTTPURLResponse, http.statusCode == 200 {
                        ok = true
                    }
                    sem.signal()
                }
                task.resume()
                _ = sem.wait(timeout: .now() + 0.6)
                
                if ok {
                    detectedIP = ip
                    break
                }
            }
            
            if let ip = detectedIP {
                if !self.isConnected || self.activeIP != ip {
                    self.enableProxy(ip: ip)
                    self.isConnected = true
                    self.activeIP = ip
                    self.notifyState(isUp: true, ip: ip)
                }
                self.fetchIPInfo(ip: ip)
            } else {
                if self.isConnected {
                    self.disableProxy()
                    self.isConnected = false
                    self.activeIP = nil
                    self.notifyState(isUp: false, ip: "")
                }
                DispatchQueue.main.async {
                    self.statusMenuItem.title = "🟡 等待手机连接..."
                    self.ipMenuItem.title = "出口 IP: 未连接"
                    self.quotaMenuItem.title = "热点配额: 未接通"
                    if let btn = self.statusItem.button {
                        btn.title = ""
                    }
                }
            }
        }
    }
    
    func enableProxy(ip: String) {
        for svc in getActiveNetworkServices() {
            let task = Process()
            task.launchPath = "/usr/sbin/networksetup"
            task.arguments = ["-setwebproxy", svc, ip, "8282"]
            try? task.run()
            task.waitUntilExit()
            
            let task2 = Process()
            task2.launchPath = "/usr/sbin/networksetup"
            task2.arguments = ["-setsecurewebproxy", svc, ip, "8282"]
            try? task2.run()
            task2.waitUntilExit()
        }
    }
    
    func disableProxy() {
        for svc in getActiveNetworkServices() {
            let task = Process()
            task.launchPath = "/usr/sbin/networksetup"
            task.arguments = ["-setwebproxystate", svc, "off"]
            try? task.run()
            task.waitUntilExit()
            
            let task2 = Process()
            task2.launchPath = "/usr/sbin/networksetup"
            task2.arguments = ["-setsecurewebproxystate", svc, "off"]
            try? task2.run()
            task2.waitUntilExit()
        }
    }
    
    func notifyState(isUp: Bool, ip: String) {
        let title = isUp ? "TetherFlow 🚀" : "TetherFlow"
        let msg = isUp ? (ip == "127.0.0.1" ? "已连接: USB 3.0 极速通道 (5 Gbps)" : "已连接: 5G Wi-Fi 满速通道") : "手机已断开，已自动恢复默认网络。"
        let task = Process()
        task.launchPath = "/usr/bin/osascript"
        task.arguments = ["-e", "display notification \"\(msg)\" with title \"\(title)\""]
        try? task.run()
    }

    func fetchIPInfo(ip: String) {
        let config = URLSessionConfiguration.ephemeral
        config.connectionProxyDictionary = [
            kCFNetworkProxiesHTTPEnable as String: true,
            kCFNetworkProxiesHTTPProxy as String: ip,
            kCFNetworkProxiesHTTPPort as String: 8282
        ]
        let session = URLSession(configuration: config)
        guard let url = URL(string: "http://ipinfo.io/json") else { return }
        
        session.dataTask(with: url) { [weak self] data, _, _ in
            guard let self = self, let data = data,
                  let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  let publicIp = json["ip"] as? String,
                  let org = json["org"] as? String else { return }
            
            let isUsb = (ip == "127.0.0.1")
            let mode = isUsb ? "USB 3.0 满速" : "Wi-Fi 6 无线"
            
            DispatchQueue.main.async {
                self.statusMenuItem.title = "🟢 5G 已直连 [\(mode)]"
                self.ipMenuItem.title = "出口 IP: \(publicIp)"
                self.quotaMenuItem.title = "运营商: \(org) (零热点配额)"
                if let btn = self.statusItem.button {
                    btn.title = isUsb ? "⚡USB" : "⚡5G"
                }
            }
        }.resume()
    }
    
    @objc func openSpeedtest() {
        if let url = URL(string: "https://www.speedtest.net") {
            NSWorkspace.shared.open(url)
        }
    }
    
    @objc func openIPInfo() {
        if let url = URL(string: "https://ipinfo.io") {
            NSWorkspace.shared.open(url)
        }
    }
    
    @objc func quitApp() {
        disableProxy()
        NSApp.terminate(nil)
    }
    
    func applicationWillTerminate(_ notification: Notification) {
        disableProxy()
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.run()

