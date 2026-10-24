import SwiftUI
import Security
import UIKit

@MainActor
final class ProxyModel: ObservableObject {
    @Published var running = false
    @Published var token = ""
    @Published var status = "Stopped"
    @Published var logs: [String] = []
    private var server: ProxyServer?
    private var generation = UUID()

    func start() {
        guard server == nil else { return }
        var bytes = [UInt8](repeating: 0, count: 32)
        guard SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) == errSecSuccess else {
            status = "Secure random generation failed"; return
        }
        token = bytes.map { String(format: "%02x", $0) }.joined()
        let current = UUID()
        generation = current
        do {
            server = try ProxyServer(token: token) { [weak self] event in
                DispatchQueue.main.async {
                    guard let self, self.generation == current else { return }
                    switch event {
                    case .listening(let port): self.status = "127.0.0.1:\(port) • cellular egress required"
                    case .log(let message):
                        self.logs.append("\(Date().formatted(date: .omitted, time: .standard)) \(message)")
                        if self.logs.count > 100 { self.logs.removeFirst(self.logs.count - 100) }
                    case .stopped:
                        self.running = false
                        self.server = nil
                        self.token = ""
                        self.status = "Stopped"
                        UIApplication.shared.isIdleTimerDisabled = false
                    }
                }
            }
            running = true
            status = "Starting loopback listener…"
            UIApplication.shared.isIdleTimerDisabled = true
            server?.start()
        } catch {
            token = ""
            status = "Configuration failed: \(error)"
        }
    }

    func stop() {
        generation = UUID()
        server?.stop()
        server = nil
        token = ""
        running = false
        status = "Stopped (foreground only)"
        UIApplication.shared.isIdleTimerDisabled = false
    }
}

@main
struct TetherFlowApp: App {
    @Environment(\.scenePhase) private var scenePhase
    @StateObject private var model = ProxyModel()

    var body: some Scene {
        WindowGroup {
            NavigationStack {
                List {
                    Section("Foreground USB prototype") {
                        Text(model.status).font(.callout.monospaced())
                        Text("Keep this app active and the phone unlocked. No background operation or bandwidth guarantee.")
                            .font(.footnote)
                        Button(model.running ? "Stop" : "Start") {
                            if model.running { model.stop() } else { model.start() }
                        }
                    }
                    if model.running {
                        Section("Session token — keep private") {
                            Text(model.token).font(.caption.monospaced()).textSelection(.enabled)
                            Button("Copy token (expires in 2 minutes)") {
                                UIPasteboard.general.setItems([[UIPasteboard.typeAutomatic: model.token]],
                                    options: [.localOnly: true, .expirationDate: Date().addingTimeInterval(120)])
                            }
                            Text("Required: Proxy-Authorization: Bearer <token>. Rotates on every Start. Loopback only, port 8283.")
                                .font(.footnote)
                        }
                    }
                    Section("Limits") {
                        Text("CONNECT :80/:443 only • 32 sessions • 64 KiB streaming chunks • local diagnostic ≤256 MiB")
                        Text("Not a VPN, PAC service, TLS interceptor, Wi-Fi/cellular aggregator, or Steam integration.")
                        Text("Diagnostic download measures host↔phone transport, NOT cellular speed.")
                    }
                    Section("Recent session logs (no tokens)") {
                        ForEach(Array(model.logs.enumerated()), id: \.offset) { _, line in
                            Text(line).font(.caption.monospaced()).textSelection(.enabled)
                        }
                    }
                }
                .navigationTitle("TetherFlow Lab")
            }
            .onChange(of: scenePhase) { _, phase in
                if phase != .active { model.stop() }
            }
        }
    }
}
