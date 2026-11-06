import SwiftUI
import Security
import UIKit

@MainActor
final class ProxyModel: ObservableObject {
    @Published var running = false
    @Published var paused = false
    /// Grouped for display; empty unless a token is live. Never logged.
    @Published var token = ""
    @Published var status = "Stopped"
    @Published var logs: [String] = []
    private var session = TokenSession(generate: ProxyModel.randomToken,
                                       now: TokenSession.monotonicNanoseconds)
    private var server: ProxyServer?
    private var generation = UUID()
    /// A token to serve once the previous server reports `.stopped` (restart is serialized).
    private var pendingToken: String?
    private var expiryTimer: Timer?

    private nonisolated static func randomToken() -> String? {
        var bytes = [UInt8](repeating: 0, count: TokenFormat.byteCount)
        guard SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) == errSecSuccess else { return nil }
        return TokenFormat.generate(bytes: bytes)
    }

    /// The token as typed on the desktop (no separators); this is also what the pasteboard receives.
    var plainToken: String { session.token ?? "" }

    func start() { apply(session.handle(.userStart)) }
    func stop() { apply(session.handle(.userStop), stoppedStatus: "Stopped (foreground only)") }
    func rotate() { apply(session.handle(.userRotate)) }
    func scene(active: Bool) { apply(session.handle(.scene(active: active))) }

    private func apply(_ action: TokenSession.Action, stoppedStatus: String = "Stopped") {
        switch action {
        case .startServer(let value):
            pendingToken = value
            if server == nil { startPending() } else { server?.stop() }  // `.stopped` starts the next one
        case .stopServer:
            pendingToken = nil
            server?.stop()
        case .none: break
        }
        refresh(stoppedStatus: stoppedStatus)
    }

    private func refresh(stoppedStatus: String = "Stopped") {
        switch session.state {
        case .running:
            running = true; paused = false
            token = TokenFormat.grouped(session.token ?? "")
            UIApplication.shared.isIdleTimerDisabled = true
            expiryTimer?.invalidate(); expiryTimer = nil
        case .paused:
            running = false; paused = true
            token = ""
            status = "Paused, will resume"
            UIApplication.shared.isIdleTimerDisabled = false
            scheduleExpiry()
        case .stopped:
            running = false; paused = false
            token = ""
            if server == nil { status = session.generationFailed ? "Secure random generation failed" : stoppedStatus }
            UIApplication.shared.isIdleTimerDisabled = false
            expiryTimer?.invalidate(); expiryTimer = nil
        }
    }

    /// Wipes an expired paused token while the app is still scheduled. A suspended process runs no
    /// timer; the next scene event enforces the window, and no background mode is requested.
    private func scheduleExpiry() {
        expiryTimer?.invalidate()
        guard let remaining = session.pauseRemainingNanoseconds() else { return }
        expiryTimer = Timer.scheduledTimer(withTimeInterval: Double(remaining) / 1e9 + 0.1, repeats: false) { [weak self] _ in
            Task { @MainActor in
                guard let self else { return }
                _ = self.session.handle(.tick)
                self.refresh()
            }
        }
    }

    private func startPending() {
        guard let value = pendingToken, server == nil else { return }
        pendingToken = nil
        let current = UUID()
        generation = current
        do {
            server = try ProxyServer(token: value) { [weak self] event in
                DispatchQueue.main.async {
                    guard let self, self.generation == current else { return }
                    switch event {
                    case .listening(let port): self.status = "127.0.0.1:\(port) • cellular egress required"
                    case .log(let message):
                        self.logs.append("\(Date().formatted(date: .omitted, time: .standard)) \(message)")
                        if self.logs.count > 100 { self.logs.removeFirst(self.logs.count - 100) }
                    case .stopped:
                        self.server = nil
                        if self.pendingToken != nil { self.startPending() }
                        else { self.refresh() }
                    }
                }
            }
            status = "Starting loopback listener…"
            server?.start()
        } catch {
            _ = session.handle(.userStop)
            status = "Configuration failed: \(error)"
            refresh(stoppedStatus: status)
        }
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
                        Button(model.running || model.paused ? "Stop" : "Start") {
                            if model.running || model.paused { model.stop() } else { model.start() }
                        }
                    }
                    if model.running {
                        Section("Session token — keep private") {
                            Text(model.token).font(.body.monospaced()).textSelection(.enabled)
                            Button("Copy token (expires in 2 minutes)") {
                                UIPasteboard.general.setItems([[UIPasteboard.typeAutomatic: model.plainToken]],
                                    options: [.localOnly: true, .expirationDate: Date().addingTimeInterval(120)])
                            }
                            Button("Rotate token") { model.rotate() }
                            Text("Required: Proxy-Authorization: Bearer <token>. Spaces and hyphens are ignored when typing it. Kept in memory only; it survives leaving the app for up to 10 minutes, and Rotate or Stop invalidates it now. Loopback only, port 8283.")
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
                model.scene(active: phase == .active)
            }
        }
    }
}
