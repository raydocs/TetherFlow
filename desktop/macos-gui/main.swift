import Cocoa
import Foundation

enum WorkMode: String {
    case smartSplit = "split"
    case dualNet    = "dual"
    case full5G     = "full"
    case homeWifi   = "home"

    var displayName: String {
        switch self {
        case .smartSplit: return "🎯 智能动静分流"
        case .dualNet:    return "⚖️ 双网并发叠加"
        case .full5G:     return "🚀 5G 极速独享"
        case .homeWifi:   return "🏠 仅家庭 Wi-Fi"
        }
    }

    var desc: String {
        switch self {
        case .smartSplit: return "游戏/通话 3ms 家庭宽带，视频/大下载 1.2G 5G"
        case .dualNet:    return "多连接下载时 Wi-Fi (150M) + 5G (1050M) 双网叠加破千兆"
        case .full5G:     return "全量流量直连 5G 满血通道 (适合测速)"
        case .homeWifi:   return "暂停 5G，全部直连家庭 Wi-Fi 路由"
        }
    }
}

class AppDelegate: NSObject, NSApplicationDelegate {
    var statusItem: NSStatusItem!
    var timer: Timer?
    var isConnected = false
    var activeIP: String? = nil
    var currentMode: WorkMode = .smartSplit
    
    // Menu items
    let statusMenuItem = NSMenuItem(title: "🟡 等待手机连接...", action: nil, keyEquivalent: "")
    let ipMenuItem = NSMenuItem(title: "出口 IP: 检测中...", action: nil, keyEquivalent: "")
    let quotaMenuItem = NSMenuItem(title: "热点配额: 0 消耗 (已绕过)", action: nil, keyEquivalent: "")

    let smartSplitMenuItem = NSMenuItem(title: "🎯 智能动静分流 (游戏3ms / 视频1.2G)", action: #selector(setModeSmartSplit), keyEquivalent: "1")
    let dualNetMenuItem    = NSMenuItem(title: "⚖️ 双网并发叠加 (下载双网叠加破千兆)", action: #selector(setModeDualNet), keyEquivalent: "2")
    let full5GMenuItem     = NSMenuItem(title: "🚀 5G 极速独享 (全量 5G 满血)", action: #selector(setModeFull5G), keyEquivalent: "3")
    let homeWifiMenuItem   = NSMenuItem(title: "🏠 仅家庭 Wi-Fi (直连路由，不走5G)", action: #selector(setModeHomeWifi), keyEquivalent: "4")

    func applicationDidFinishLaunching(_ notification: Notification) {
        if let savedMode = UserDefaults.standard.string(forKey: "TetherFlow_WorkMode"),
           let mode = WorkMode(rawValue: savedMode) {
            currentMode = mode
        }
        setupStatusItem()
        updateModeMenuState()
        applyBrowserSpeedOptimizations()
        ensureInstalledAndAutoStart()
        startDaemonAndMonitor()
    }

    func ensureInstalledAndAutoStart() {
        let fileManager = FileManager.default
        let currentPath = Bundle.main.bundlePath
        let appDestPath = "/Applications/TetherFlow.app"

        if currentPath != appDestPath && !currentPath.hasPrefix("/Applications/") {
            try? fileManager.removeItem(atPath: appDestPath)
            try? fileManager.copyItem(atPath: currentPath, toPath: appDestPath)
        }

        let script = "tell application \"System Events\" to if not (exists (login item \"TetherFlow\")) then make login item at end with properties {path:\"\(appDestPath)\", hidden:true}"
        runCommand("/usr/bin/osascript", ["-e", script])
    }

    func applyBrowserSpeedOptimizations() {
        runCommand("/bin/sh", ["-c", "defaults write com.google.Chrome DisableQuic -bool true; defaults write com.microsoft.Edge DisableQuic -bool true; defaults write com.brave.Browser DisableQuic -bool true"])
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

        let modeHeader = NSMenuItem(title: "─── 工作模式切换 ───", action: nil, keyEquivalent: "")
        modeHeader.isEnabled = false
        menu.addItem(modeHeader)
        menu.addItem(smartSplitMenuItem)
        menu.addItem(dualNetMenuItem)
        menu.addItem(full5GMenuItem)
        menu.addItem(homeWifiMenuItem)

        menu.addItem(NSMenuItem.separator())
        menu.addItem(NSMenuItem(title: "🚀 打开 Speedtest 测速", action: #selector(openSpeedtest), keyEquivalent: "s"))
        menu.addItem(NSMenuItem(title: "🌐 打开 IP 检测 (ipinfo.io)", action: #selector(openIPInfo), keyEquivalent: "i"))
        menu.addItem(NSMenuItem.separator())
        menu.addItem(NSMenuItem(title: "退出 TetherFlow", action: #selector(quitApp), keyEquivalent: "q"))
        statusItem.menu = menu
    }

    func updateModeMenuState() {
        smartSplitMenuItem.state = (currentMode == .smartSplit) ? .on : .off
        dualNetMenuItem.state    = (currentMode == .dualNet)    ? .on : .off
        full5GMenuItem.state     = (currentMode == .full5G)     ? .on : .off
        homeWifiMenuItem.state   = (currentMode == .homeWifi)   ? .on : .off
    }

    @objc func setModeSmartSplit() { switchMode(to: .smartSplit) }
    @objc func setModeDualNet()    { switchMode(to: .dualNet) }
    @objc func setModeFull5G()     { switchMode(to: .full5G) }
    @objc func setModeHomeWifi()   { switchMode(to: .homeWifi) }

    func switchMode(to mode: WorkMode) {
        currentMode = mode
        UserDefaults.standard.set(mode.rawValue, forKey: "TetherFlow_WorkMode")
        updateModeMenuState()

        if isConnected, let ip = activeIP {
            applyProxyForCurrentMode(ip: ip)
        }
        notifyState(isUp: true, ip: activeIP ?? "127.0.0.1", customMsg: "已切换为: \(mode.displayName)\n\(mode.desc)")
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
                runCommand(p, ["forward", "tcp:8282", "tcp:8282"])
                break
            }
        }
    }

    func getDefaultGateway() -> String? {
        let pipe = Pipe()
        let task = Process()
        task.launchPath = "/bin/sh"
        task.arguments = ["-c", "route -n get default | awk '/gateway/{print $2}'"]
        task.standardOutput = pipe
        try? task.run()
        task.waitUntilExit()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        let str = String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
        return (str != nil && !str!.isEmpty) ? str : nil
    }

    func getActiveNetworkServices() -> [String] {
        let pipe = Pipe()
        let task = Process()
        task.launchPath = "/usr/sbin/networksetup"
        task.arguments = ["-listallnetworkservices"]
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
                    self.applyProxyForCurrentMode(ip: ip)
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
    
    func applyProxyForCurrentMode(ip: String) {
        let services = getActiveNetworkServices()
        switch currentMode {
        case .smartSplit:
            let pacUrl = "http://\(ip):8282/pac?mode=split"
            for svc in services {
                runCommand("/usr/sbin/networksetup", ["-setwebproxystate", svc, "off"])
                runCommand("/usr/sbin/networksetup", ["-setsecurewebproxystate", svc, "off"])
                runCommand("/usr/sbin/networksetup", ["-setautoproxyurl", svc, pacUrl])
                runCommand("/usr/sbin/networksetup", ["-setautoproxystate", svc, "on"])
                runCommand("/usr/sbin/networksetup", ["-setproxybypassdomains", svc, "127.0.0.1", "192.168.0.0/16", "10.0.0.0/8", "*.local", "<local>"])
            }
        case .dualNet:
            let pacUrl = "http://\(ip):8282/pac?mode=dual"
            for svc in services {
                runCommand("/usr/sbin/networksetup", ["-setwebproxystate", svc, "off"])
                runCommand("/usr/sbin/networksetup", ["-setsecurewebproxystate", svc, "off"])
                runCommand("/usr/sbin/networksetup", ["-setautoproxyurl", svc, pacUrl])
                runCommand("/usr/sbin/networksetup", ["-setautoproxystate", svc, "on"])
                runCommand("/usr/sbin/networksetup", ["-setproxybypassdomains", svc, "127.0.0.1", "192.168.0.0/16", "10.0.0.0/8", "*.local", "<local>"])
            }
        case .full5G:
            for svc in services {
                runCommand("/usr/sbin/networksetup", ["-setautoproxystate", svc, "off"])
                runCommand("/usr/sbin/networksetup", ["-setwebproxy", svc, ip, "8282"])
                runCommand("/usr/sbin/networksetup", ["-setsecurewebproxy", svc, ip, "8282"])
                runCommand("/usr/sbin/networksetup", ["-setwebproxystate", svc, "on"])
                runCommand("/usr/sbin/networksetup", ["-setsecurewebproxystate", svc, "on"])
                runCommand("/usr/sbin/networksetup", ["-setproxybypassdomains", svc, "127.0.0.1", "localhost", "<local>"])
            }
        case .homeWifi:
            disableProxy()
        }
    }
    
    func disableProxy() {
        for svc in getActiveNetworkServices() {
            runCommand("/usr/sbin/networksetup", ["-setwebproxystate", svc, "off"])
            runCommand("/usr/sbin/networksetup", ["-setsecurewebproxystate", svc, "off"])
            runCommand("/usr/sbin/networksetup", ["-setautoproxystate", svc, "off"])
        }
    }

    func runCommand(_ path: String, _ args: [String]) {
        let task = Process()
        task.launchPath = path
        task.arguments = args
        try? task.run()
        task.waitUntilExit()
    }
    
    func notifyState(isUp: Bool, ip: String, customMsg: String? = nil) {
        let title = isUp ? "TetherFlow 🚀" : "TetherFlow"
        let msg: String
        if let cm = customMsg {
            msg = cm
        } else if isUp {
            let link = (ip == "127.0.0.1") ? "USB 3.0 直连 (5 Gbps)" : "5G Wi-Fi 满速"
            msg = "已连接: \(link)\n模式: \(currentMode.displayName)"
        } else {
            msg = "手机已断开，已自动恢复默认网络。"
        }
        let escaped = msg.replacingOccurrences(of: "\"", with: "\\\"")
        runCommand("/usr/bin/osascript", ["-e", "display notification \"\(escaped)\" with title \"\(title)\""])
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
            let linkName = isUsb ? "USB 3.0" : "Wi-Fi 6"
            
            DispatchQueue.main.async {
                self.statusMenuItem.title = "🟢 已接入 [\(linkName)] - \(self.currentMode.displayName)"
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

