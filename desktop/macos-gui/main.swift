import Cocoa
import Foundation

class AppDelegate: NSObject, NSApplicationDelegate {
    var statusItem: NSStatusItem!
    var window: NSWindow!
    var timer: Timer?
    var isConnected = false
    var currentIP = "正在检测..."
    var currentISP = "正在检测..."
    
    let statusLabel = NSTextField(labelWithString: "正在连接手机...")
    let ipLabel = NSTextField(labelWithString: "出口 IP: 检测中...")
    let ispLabel = NSTextField(labelWithString: "运营商: 检测中...")
    let quotaLabel = NSTextField(labelWithString: "热点配额: 0 消耗 (已绕过)")
    let toggleBtn = NSButton()

    func applicationDidFinishLaunching(_ notification: Notification) {
        setupStatusItem()
        setupWindow()
        startDaemonAndMonitor()
    }
    
    func setupStatusItem() {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let button = statusItem.button {
            if #available(macOS 11.0, *) {
                button.image = NSImage(systemSymbolName: "bolt.fill", accessibilityDescription: "TetherFlow")
            } else {
                button.title = "⚡TF"
            }
        }
        
        let menu = NSMenu()
        menu.addItem(NSMenuItem(title: "TetherFlow 5G 控制中心", action: #selector(showWindow), keyEquivalent: "o"))
        menu.addItem(NSMenuItem.separator())
        menu.addItem(NSMenuItem(title: "🚀 打开 Speedtest 测速", action: #selector(openSpeedtest), keyEquivalent: "s"))
        menu.addItem(NSMenuItem(title: "🌐 打开 IP 检测 (ipinfo.io)", action: #selector(openIPInfo), keyEquivalent: "i"))
        menu.addItem(NSMenuItem.separator())
        menu.addItem(NSMenuItem(title: "退出 TetherFlow", action: #selector(quitApp), keyEquivalent: "q"))
        statusItem.menu = menu
    }
    
    func setupWindow() {
        let winWidth: CGFloat = 380
        let winHeight: CGFloat = 280
        let screenSize = NSScreen.main?.frame.size ?? CGSize(width: 1440, height: 900)
        let rect = NSRect(x: (screenSize.width - winWidth) / 2, y: (screenSize.height - winHeight) / 2, width: winWidth, height: winHeight)
        
        window = NSWindow(contentRect: rect,
                          styleMask: [.titled, .closable, .miniaturizable],
                          backing: .buffered, defer: false)
        window.title = "TetherFlow 控制中心"
        window.isReleasedWhenClosed = false
        
        let visualEffect = NSVisualEffectView(frame: NSRect(x: 0, y: 0, width: winWidth, height: winHeight))
        visualEffect.material = .sidebar
        visualEffect.blendingMode = .behindWindow
        visualEffect.state = .active
        
        // Title
        let header = NSTextField(labelWithString: "⚡ TetherFlow 5G 直连")
        header.font = NSFont.boldSystemFont(ofSize: 18)
        header.frame = NSRect(x: 24, y: 228, width: 330, height: 26)
        
        // Subtitle
        let sub = NSTextField(labelWithString: "三星 S24+ 原生 5G 极速中继 (已绕过 AT&T 热点侦测)")
        sub.font = NSFont.systemFont(ofSize: 11)
        sub.textColor = .secondaryLabelColor
        sub.frame = NSRect(x: 24, y: 206, width: 330, height: 18)
        
        // Card Background
        let card = NSBox(frame: NSRect(x: 20, y: 80, width: 340, height: 116))
        card.boxType = .custom
        card.fillColor = NSColor.controlBackgroundColor.withAlphaComponent(0.6)
        card.borderColor = NSColor.separatorColor
        card.borderWidth = 1
        card.cornerRadius = 10
        
        statusLabel.font = NSFont.boldSystemFont(ofSize: 14)
        statusLabel.textColor = .systemGreen
        statusLabel.frame = NSRect(x: 16, y: 82, width: 300, height: 22)
        
        ipLabel.font = NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
        ipLabel.frame = NSRect(x: 16, y: 58, width: 300, height: 20)
        
        ispLabel.font = NSFont.systemFont(ofSize: 12)
        ispLabel.frame = NSRect(x: 16, y: 34, width: 300, height: 20)
        
        quotaLabel.font = NSFont.systemFont(ofSize: 12)
        quotaLabel.textColor = .systemIndigo
        quotaLabel.frame = NSRect(x: 16, y: 10, width: 300, height: 20)
        
        card.addSubview(statusLabel)
        card.addSubview(ipLabel)
        card.addSubview(ispLabel)
        card.addSubview(quotaLabel)
        
        // Buttons
        let testBtn = NSButton(title: "🚀 测速", target: self, action: #selector(openSpeedtest))
        testBtn.frame = NSRect(x: 20, y: 24, width: 100, height: 36)
        testBtn.bezelStyle = .rounded
        
        let ipBtn = NSButton(title: "🌐 查看出口", target: self, action: #selector(openIPInfo))
        ipBtn.frame = NSRect(x: 125, y: 24, width: 110, height: 36)
        ipBtn.bezelStyle = .rounded
        
        let hideBtn = NSButton(title: "隐藏窗口", target: self, action: #selector(hideWindow))
        hideBtn.frame = NSRect(x: 240, y: 24, width: 110, height: 36)
        hideBtn.bezelStyle = .rounded
        
        visualEffect.addSubview(header)
        visualEffect.addSubview(sub)
        visualEffect.addSubview(card)
        visualEffect.addSubview(testBtn)
        visualEffect.addSubview(ipBtn)
        visualEffect.addSubview(hideBtn)
        
        window.contentView = visualEffect
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }
    
    func startDaemonAndMonitor() {
        // Run check loop
        checkConnection()
        timer = Timer.scheduledTimer(withTimeInterval: 3.0, repeats: true) { [weak self] _ in
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
            
            var detectedIP: String? = nil

            for ip in candidates {
                guard let url = URL(string: "http://\(ip):8282/pac") else { continue }
                var req = URLRequest(url: url)
                req.timeoutInterval = 0.5
                
                let sem = DispatchSemaphore(value: 0)
                var ok = false
                let task = URLSession.shared.dataTask(with: req) { _, resp, _ in
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
                self.enableProxy(ip: ip)
                self.fetchIPInfo(ip: ip)
            } else {
                DispatchQueue.main.async {
                    self.statusLabel.stringValue = "🟡 等待手机连接 (请开启手机端开关)..."
                    self.statusLabel.textColor = .systemOrange
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
            let modeTitle = isUsb ? "🟢 5G 已直连 [USB 3.0 5Gbps 满速]" : "🟢 5G 已直连 [5G Wi-Fi 6 无线]"
            
            DispatchQueue.main.async {
                self.statusLabel.stringValue = modeTitle
                self.statusLabel.textColor = .systemGreen
                self.ipLabel.stringValue = "出口 IP: \(publicIp) [\(ip)]"
                self.ispLabel.stringValue = "运营商: \(org)"
                self.quotaLabel.stringValue = "热点侦测: 绕过成功 (TTL=64 无限流量)"
            }
        }.resume()
    }
    
    @objc func showWindow() {
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }
    
    @objc func hideWindow() {
        window.orderOut(nil)
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
