import Foundation
import Network

/// Internal transport seam: production always uses cellular; host tests can use a loopback peer.
struct EgressPolicy {
    let connection: (Authority) -> NWConnection
    let permits: (NWPath) -> Bool

    static let cellular = EgressPolicy(connection: { authority in
        let tcp = NWProtocolTCP.Options()
        tcp.noDelay = true
        let parameters = NWParameters(tls: nil, tcp: tcp)
        parameters.requiredInterfaceType = .cellular
        parameters.multipathServiceType = .disabled
        return NWConnection(host: NWEndpoint.Host(authority.host),
                            port: NWEndpoint.Port(rawValue: authority.port)!, using: parameters)
    }, permits: { $0.status == .satisfied && $0.usesInterfaceType(.cellular) })
}

public enum ProxyEvent {
    case listening(UInt16)
    case log(String)
    case stopped
}

/// All socket/session state is confined to one serial queue, including cancellation.
public final class ProxyServer {
    public static let defaultPort: UInt16 = 8283
    private let queue = DispatchQueue(label: "TetherFlow.Proxy")
    private let token: String
    private let port: UInt16
    private let egress: EgressPolicy
    private let event: (ProxyEvent) -> Void
    private var listener: NWListener?
    private var sessions: [UUID: ProxySession] = [:]
    public static let maximumSessions = 32

    public convenience init(token: String, port: UInt16 = ProxyServer.defaultPort,
                            event: @escaping (ProxyEvent) -> Void) throws {
        try self.init(token: token, port: port, egress: .cellular, event: event)
    }

    init(token: String, port: UInt16, egress: EgressPolicy,
         event: @escaping (ProxyEvent) -> Void) throws {
        guard token.utf8.count >= 32, token.utf8.count <= 128,
              token.utf8.allSatisfy({ (33...126).contains($0) }) else { throw ProtocolFailure.malformed }
        self.token = token
        self.port = port
        self.egress = egress
        self.event = event
    }

    public func start() {
        queue.async { [self] in
            guard listener == nil else { return }
            do {
                let parameters = NWParameters.tcp
                // Never wildcard-bind or advertise Bonjour. usbmux/iproxy reaches device loopback.
                parameters.requiredLocalEndpoint = .hostPort(host: "127.0.0.1",
                                                             port: NWEndpoint.Port(rawValue: port)!)
                let newListener = try NWListener(using: parameters)
                listener = newListener
                newListener.stateUpdateHandler = { [weak self, weak newListener] state in
                    guard let self, let newListener, self.listener === newListener else { return }
                    switch state {
                    case .ready:
                        guard let boundPort = newListener.port else { self.stopOnQueue(); return }
                        self.event(.listening(boundPort.rawValue))
                    case .failed(let error):
                        self.event(.log("Listener failed: \(error)"))
                        self.stopOnQueue()
                    default: break
                    }
                }
                let callbackQueue = queue
                newListener.newConnectionHandler = { [weak self, weak newListener] connection in
                    guard let self, let newListener, self.listener === newListener,
                          self.sessions.count < Self.maximumSessions else {
                        // Even rejected setup-state connections need a queue to deliver cancellation.
                        connection.start(queue: callbackQueue)
                        connection.cancel()
                        return
                    }
                    let id = UUID()
                    let session = ProxySession(client: connection, token: self.token, queue: self.queue,
                                               egress: self.egress) { [weak self] message in
                        self?.sessions.removeValue(forKey: id)
                        self?.event(.log(message))
                    }
                    self.sessions[id] = session
                    session.start()
                    self.event(.log("Session opened (active=\(self.sessions.count))"))
                }
                newListener.start(queue: queue)
            } catch {
                event(.log("Listener creation failed: \(error)"))
                event(.stopped)
            }
        }
    }

    public func stop() { queue.async { [self] in stopOnQueue() } }

    private func stopOnQueue() {
        listener?.stateUpdateHandler = nil
        // Keep the reject handler until cancellation: queued accepts must still be closed.
        listener?.cancel()
        listener = nil
        let active = Array(sessions.values)
        active.forEach { $0.cancel(reason: "server stopped") }
        sessions.removeAll()
        event(.stopped)
    }

    deinit {
        let lastListener = listener
        let active = Array(sessions.values)
        queue.async {
            lastListener?.stateUpdateHandler = nil
            lastListener?.cancel()
            active.forEach { $0.dispose() }
        }
    }
}

private final class ProxySession {
    private enum Phase { case headers, connecting, tunnel, diagnostic, closing, finished }
    private let client: NWConnection
    private var upstream: NWConnection?
    private let token: String
    private let queue: DispatchQueue
    private let egress: EgressPolicy
    private let completion: (String) -> Void
    private var parser = HeaderParser()
    private var phase: Phase = .headers
    private var timer: DispatchSourceTimer?
    private let created = DispatchTime.now().uptimeNanoseconds
    private var phaseStart = DispatchTime.now().uptimeNanoseconds
    private var activity = DispatchTime.now().uptimeNanoseconds
    private var uploadBytes = 0
    private var downloadBytes = 0
    private var finishedDirections = 0

    init(client: NWConnection, token: String, queue: DispatchQueue, egress: EgressPolicy,
         completion: @escaping (String) -> Void) {
        self.client = client
        self.token = token
        self.queue = queue
        self.egress = egress
        self.completion = completion
    }

    func start() {
        let timer = DispatchSource.makeTimerSource(queue: queue)
        self.timer = timer
        timer.schedule(deadline: .now() + 1, repeating: 1)
        timer.setEventHandler { [weak self] in self?.checkDeadline() }
        timer.resume()
        client.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            switch state {
            case .ready: self.readHeader()
            case .failed: self.cancel(reason: "client failed")
            case .cancelled: self.cancel(reason: "client cancelled")
            default: break
            }
        }
        client.start(queue: queue)
    }

    func dispose() {
        timer?.setEventHandler {}
        timer?.cancel()
        timer = nil
        client.stateUpdateHandler = nil
        upstream?.stateUpdateHandler = nil
        upstream?.pathUpdateHandler = nil
        client.cancel()
        upstream?.cancel()
    }

    func cancel(reason: String) {
        guard phase != .finished else { return }
        phase = .finished
        dispose()
        let seconds = Double(DispatchTime.now().uptimeNanoseconds - created) / 1e9
        completion(String(format: "%@; up=%d down=%d elapsed=%.3fs", reason, uploadBytes, downloadBytes, seconds))
    }

    private func transition(_ phase: Phase) {
        self.phase = phase
        phaseStart = DispatchTime.now().uptimeNanoseconds
        activity = phaseStart
    }

    private func checkDeadline() {
        let now = DispatchTime.now().uptimeNanoseconds
        let age = Double(now - phaseStart) / 1e9
        let idle = Double(now - activity) / 1e9
        switch phase {
        case .headers where age >= 10: respond(408, "Request Timeout")
        case .connecting where age >= 15: respond(504, "Gateway Timeout")
        case .diagnostic where age >= 60: cancel(reason: "diagnostic deadline")
        case .closing where age >= 5: cancel(reason: "close deadline")
        case .tunnel, .diagnostic:
            if idle >= 30 { cancel(reason: "idle timeout") }
        default: break
        }
    }

    private func readHeader() {
        guard phase == .headers else { return }
        client.receive(minimumIncompleteLength: 1, maximumLength: DiagnosticPayload.chunkBytes) { [weak self] data, _, eof, error in
            guard let self, self.phase == .headers else { return }
            guard error == nil else { self.cancel(reason: "header receive failed"); return }
            do {
                if let data, !data.isEmpty, let (request, trailing) = try self.parser.append(data) {
                    guard request.authenticated(by: self.token) else {
                        self.respond(407, "Proxy Authentication Required", extra: "Proxy-Authenticate: Bearer realm=\"TetherFlow\"\r\n")
                        return
                    }
                    switch try request.route() {
                    case .connect(let authority): self.connect(authority, initial: trailing, clientEOF: eof)
                    case .download(let count):
                        guard trailing.isEmpty else { self.respond(400, "Bad Request"); return }
                        self.download(count)
                    }
                    return
                }
                if eof { self.cancel(reason: "incomplete header EOF") } else { self.readHeader() }
            } catch ProtocolFailure.headerTooLarge { self.respond(431, "Request Header Fields Too Large")
            } catch ProtocolFailure.forbiddenPort { self.respond(403, "Forbidden")
            } catch ProtocolFailure.unsupported { self.respond(405, "Method Not Allowed")
            } catch { self.respond(400, "Bad Request") }
        }
    }

    private func respond(_ status: Int, _ phrase: String, extra: String = "") {
        guard phase != .finished, phase != .closing else { return }
        transition(.closing)
        upstream?.cancel()
        let response = "HTTP/1.1 \(status) \(phrase)\r\n\(extra)Content-Length: 0\r\nConnection: close\r\n\r\n"
        client.send(content: Data(response.utf8), completion: .contentProcessed { [weak self] _ in
            self?.cancel(reason: "HTTP \(status)")
        })
    }

    private func connect(_ authority: Authority, initial: Data, clientEOF: Bool) {
        transition(.connecting)
        let connection = egress.connection(authority)
        upstream = connection
        connection.pathUpdateHandler = { [weak self] path in
            guard let self, self.phase == .tunnel || self.phase == .connecting else { return }
            if !self.egress.permits(path) {
                if self.phase == .connecting { self.respond(502, "Bad Gateway") }
                else { self.cancel(reason: "non-cellular or unsatisfied path rejected") }
            }
        }
        connection.stateUpdateHandler = { [weak self] state in
            guard let self, self.phase == .connecting || self.phase == .tunnel else { return }
            switch state {
            case .ready:
                guard self.phase == .connecting else { return }
                guard let path = connection.currentPath, self.egress.permits(path) else {
                    self.respond(502, "Bad Gateway"); return
                }
                self.transition(.tunnel)
                self.client.send(content: Data("HTTP/1.1 200 Connection Established\r\n\r\n".utf8),
                                 completion: .contentProcessed { [weak self] error in
                    guard let self, self.phase == .tunnel else { return }
                    guard error == nil else { self.cancel(reason: "CONNECT response failed"); return }
                    self.forward(initial, eof: clientEOF, from: self.client, to: connection, uploading: true)
                    self.pump(from: connection, to: self.client, uploading: false)
                })
            case .failed:
                if self.phase == .connecting { self.respond(502, "Bad Gateway") }
                else { self.cancel(reason: "upstream failed") }
            default: break
            }
        }
        connection.start(queue: queue)
    }

    private func pump(from source: NWConnection, to destination: NWConnection, uploading: Bool) {
        guard phase == .tunnel else { return }
        source.receive(minimumIncompleteLength: 1, maximumLength: DiagnosticPayload.chunkBytes) { [weak self] data, _, eof, error in
            guard let self, self.phase == .tunnel else { return }
            guard error == nil else { self.cancel(reason: "relay receive failed"); return }
            self.activity = DispatchTime.now().uptimeNanoseconds
            self.forward(data ?? Data(), eof: eof, from: source, to: destination, uploading: uploading)
        }
    }

    private func forward(_ data: Data, eof: Bool, from source: NWConnection,
                         to destination: NWConnection, uploading: Bool) {
        guard phase == .tunnel else { return }
        // Wait for contentProcessed before the next receive: at most one 64 KiB send per direction.
        if data.isEmpty {
            if eof { halfClose(destination) } else { pump(from: source, to: destination, uploading: uploading) }
            return
        }
        destination.send(content: data, completion: .contentProcessed { [weak self] error in
            guard let self, self.phase == .tunnel else { return }
            guard error == nil else { self.cancel(reason: "relay send failed"); return }
            if uploading { self.uploadBytes += data.count } else { self.downloadBytes += data.count }
            self.activity = DispatchTime.now().uptimeNanoseconds
            if eof { self.halfClose(destination) }
            else { self.pump(from: source, to: destination, uploading: uploading) }
        })
    }

    private func halfClose(_ connection: NWConnection) {
        connection.send(content: nil, contentContext: .finalMessage, isComplete: true,
                        completion: .contentProcessed { [weak self] error in
            guard let self, self.phase == .tunnel else { return }
            guard error == nil else { self.cancel(reason: "half-close failed"); return }
            self.finishedDirections += 1
            if self.finishedDirections == 2 { self.cancel(reason: "tunnel complete") }
        })
    }

    private func download(_ count: Int) {
        transition(.diagnostic)
        let header = "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: \(count)\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n"
        client.send(content: Data(header.utf8), completion: .contentProcessed { [weak self] error in
            guard let self, self.phase == .diagnostic else { return }
            guard error == nil else { self.cancel(reason: "diagnostic header failed"); return }
            self.sendDiagnostic(remaining: count)
        })
    }

    private func sendDiagnostic(remaining: Int) {
        guard phase == .diagnostic else { return }
        guard remaining > 0 else { cancel(reason: "diagnostic complete"); return }
        let count = min(remaining, DiagnosticPayload.pattern.count)
        let data = DiagnosticPayload.pattern.prefix(count)
        client.send(content: data, completion: .contentProcessed { [weak self] error in
            guard let self, self.phase == .diagnostic else { return }
            guard error == nil else { self.cancel(reason: "diagnostic send failed"); return }
            self.downloadBytes += count
            self.activity = DispatchTime.now().uptimeNanoseconds
            self.sendDiagnostic(remaining: remaining - count)
        })
    }
}
