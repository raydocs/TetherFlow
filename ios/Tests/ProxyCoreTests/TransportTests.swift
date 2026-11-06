import XCTest
import Network
import Darwin
@testable import ProxyCore

private enum SocketFailure: Error { case syscall(String), prematureEOF }

private func openSocket(port: UInt16) throws -> Int32 {
    let fd = Darwin.socket(AF_INET, SOCK_STREAM, 0)
    guard fd >= 0 else { throw SocketFailure.syscall("socket") }
    var timeout = timeval(tv_sec: 8, tv_usec: 0)
    var one: Int32 = 1
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout)))
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout)))
    setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout.size(ofValue: one)))
    var address = sockaddr_in()
    address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
    address.sin_family = sa_family_t(AF_INET)
    address.sin_port = port.bigEndian
    address.sin_addr.s_addr = inet_addr("127.0.0.1")
    let result = withUnsafePointer(to: &address) { pointer in
        pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
        }
    }
    guard result == 0 else { Darwin.close(fd); throw SocketFailure.syscall("connect: \(errno)") }
    return fd
}

private func writeAll(_ fd: Int32, _ data: Data) throws {
    try data.withUnsafeBytes { bytes in
        var offset = 0
        while offset < bytes.count {
            let count = Darwin.send(fd, bytes.baseAddress!.advanced(by: offset), bytes.count - offset, 0)
            guard count > 0 else { throw SocketFailure.syscall("send: \(errno)") }
            offset += count
        }
    }
}

private func readHeader(_ fd: Int32) throws -> String {
    var data = Data()
    while !data.suffix(4).elementsEqual([13, 10, 13, 10]) {
        var byte: UInt8 = 0
        guard Darwin.recv(fd, &byte, 1, 0) == 1 else { throw SocketFailure.prematureEOF }
        data.append(byte)
        guard data.count <= HeaderParser.maximumBytes else { throw ProtocolFailure.headerTooLarge }
    }
    return String(decoding: data, as: UTF8.self)
}

private func readUntilEOF(_ fd: Int32) throws -> Data {
    var output = Data()
    var chunk = [UInt8](repeating: 0, count: 32768)
    while true {
        let count = Darwin.recv(fd, &chunk, chunk.count, 0)
        if count == 0 { return output }
        guard count > 0 else { throw SocketFailure.syscall("recv: \(errno)") }
        output.append(contentsOf: chunk.prefix(count))
        guard output.count <= 8 * 1024 * 1024 else { throw ProtocolFailure.invalidByteCount }
    }
}

private final class Locked {
    private let lock = NSCondition()
    private var marks: [String] = []
    func mark(_ name: String) { lock.lock(); marks.append(name); lock.broadcast(); lock.unlock() }
    func count(_ name: String) -> Int { lock.lock(); defer { lock.unlock() }; return marks.filter { $0 == name }.count }
    func wait(forAny names: [String], timeout: TimeInterval) -> String? {
        let deadline = Date().addingTimeInterval(timeout)
        lock.lock(); defer { lock.unlock() }
        while true {
            if let hit = marks.first(where: names.contains) { return hit }
            if !lock.wait(until: deadline) { return nil }
        }
    }
}

final class TransportTests: XCTestCase {
    let token = String(repeating: "c", count: 64)
    private var servers: [ProxyServer] = []

    override func tearDown() {
        for server in servers { server.stop() }
        servers.removeAll()
        super.tearDown()
    }

    private func start(egress: EgressPolicy = .cellular, opened: XCTestExpectation? = nil) throws -> (ProxyServer, UInt16) {
        let ready = expectation(description: "loopback listener ready")
        var port: UInt16 = 0
        let server = try ProxyServer(token: token, port: 0, egress: egress) { event in
            if case .listening(let bound) = event { port = bound; ready.fulfill() }
            if case .log(let message) = event, message.hasPrefix("Session opened") { opened?.fulfill() }
        }
        servers.append(server)
        server.start()
        wait(for: [ready], timeout: 5)
        XCTAssertNotEqual(port, 0)
        return (server, port)
    }

    private func request(_ target: String, method: String = "GET", authenticated: Bool = true) -> Data {
        Data("\(method) \(target) HTTP/1.1\r\nHost: localhost\r\n\(authenticated ? "Proxy-Authorization: Bearer \(token)\r\n" : "")\r\n".utf8)
    }

    /// R8: `.stopped` must mean the listener is fully released, so an immediate restart on the same
    /// fixed port never hits EADDRINUSE. Scratch probe before the fix: 16/50 rebinds failed.
    func testImmediateRestartSamePortAfterStopped() throws {
        var port: UInt16 = 0
        do { // find a free port once, then reuse exactly that port for every cycle
            let (probe, bound) = try start()
            port = bound
            let released = expectation(description: "probe port released")
            probe.stop()
            DispatchQueue.global().asyncAfter(deadline: .now() + 0.5) { released.fulfill() }
            wait(for: [released], timeout: 2)
        }
        var failures: [String] = []
        for cycle in 0..<50 {
            let state = Locked()
            let server = try ProxyServer(token: token, port: port, egress: .cellular) { event in
                switch event {
                case .listening: state.mark("listening")
                case .stopped: state.mark("stopped")
                case .log(let message): if message.hasPrefix("Listener") { state.mark("failed") }
                }
            }
            server.start()
            let bound = state.wait(forAny: ["listening", "failed"], timeout: 5)
            if bound != "listening" { failures.append("cycle \(cycle): \(bound ?? "timeout")") }
            // A client mid-request when the user stops, as in the app.
            if bound == "listening", let fd = try? openSocket(port: port) {
                try? writeAll(fd, Data("GET /diag".utf8))
                server.stop()
                Darwin.close(fd)
            } else {
                server.stop()
            }
            if state.wait(forAny: ["stopped"], timeout: 5) == nil { failures.append("cycle \(cycle): no .stopped") }
        }
        XCTAssertEqual(failures, [], "same-port restart failed \(failures.count)/50 cycles")
    }

    /// R8 wire check: after a rotate the old token gets 407 and the new one is served on the same port.
    func testOldTokenRejectedOnWireAfterRotate() throws {
        var counter = 0
        var session = TokenSession(generate: { counter += 1; return TokenFormat.generate(bytes: [UInt8](repeating: UInt8(counter), count: 20)) },
                                   now: { 1 })
        guard case .startServer(let first) = session.handle(.userStart) else { return XCTFail("no token") }
        let state = Locked()
        var port: UInt16 = 0
        let one = try ProxyServer(token: first, port: 0, egress: .cellular) { event in
            if case .listening(let bound) = event { port = bound; state.mark("listening") }
            if case .stopped = event { state.mark("stopped") }
        }
        servers.append(one)
        one.start()
        XCTAssertEqual(state.wait(forAny: ["listening"], timeout: 5), "listening")
        func status(_ token: String) throws -> String {
            let fd = try openSocket(port: port)
            defer { Darwin.close(fd) }
            try writeAll(fd, Data("GET /diagnostic/info HTTP/1.1\r\nHost: localhost\r\nProxy-Authorization: Bearer \(token)\r\n\r\n".utf8))
            return String(try readHeader(fd).prefix(12))
        }
        XCTAssertEqual(try status(first), "HTTP/1.1 200")

        guard case .startServer(let second) = session.handle(.userRotate) else { return XCTFail("no rotate") }
        XCTAssertNotEqual(first, second)
        one.stop()  // the app stops, waits for `.stopped`, then starts the replacement on the same port
        XCTAssertEqual(state.wait(forAny: ["stopped"], timeout: 5), "stopped")
        let state2 = Locked()
        let two = try ProxyServer(token: second, port: port, egress: .cellular) { event in
            if case .listening = event { state2.mark("listening") }
        }
        servers.append(two)
        two.start()
        XCTAssertEqual(state2.wait(forAny: ["listening"], timeout: 5), "listening")
        XCTAssertEqual(try status(first), "HTTP/1.1 407", "the rotated-out token must be rejected")
        XCTAssertEqual(try status(second), "HTTP/1.1 200")
    }

    /// `.stopped` is reported once per stop, and a start requested mid-stop waits for it (serialized).
    func testStopReportedOnceAndStartDuringStopIsSerialized() throws {
        let state = Locked()
        let server = try ProxyServer(token: token, port: 0, egress: .cellular) { event in
            switch event {
            case .listening: state.mark("listening")
            case .stopped: state.mark("stopped")
            case .log: break
            }
        }
        servers.append(server)
        server.start()
        XCTAssertEqual(state.wait(forAny: ["listening"], timeout: 5), "listening")
        server.stop()
        server.stop()
        server.start()  // arrives while the first stop may still be draining
        XCTAssertEqual(state.wait(forAny: ["stopped"], timeout: 5), "stopped")
        let deadline = Date().addingTimeInterval(5)
        while state.count("listening") < 2, Date() < deadline { Thread.sleep(forTimeInterval: 0.01) }
        XCTAssertEqual(state.count("listening"), 2, "the queued start ran after the stop completed")
        XCTAssertEqual(state.count("stopped"), 1, "two stop() calls report .stopped once")
    }

    func testDiagnosticActualTCPExactBytesAndEOF() throws {
        let (_, port) = try start()
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        let count = 2 * 1024 * 1024 + 137
        let bytes = request("/diagnostic/download?bytes=\(count)")
        // Exercise partial headers on the actual listener, not just the parser.
        try writeAll(fd, Data(bytes.prefix(11)))
        try writeAll(fd, Data(bytes.dropFirst(11)))
        let header = try readHeader(fd)
        XCTAssertTrue(header.hasPrefix("HTTP/1.1 200 OK"))
        XCTAssertTrue(header.contains("Content-Length: \(count)\r\n"))
        let body = try readUntilEOF(fd)
        XCTAssertEqual(body, Data((0..<count).map { UInt8($0 % 251) }))
    }

    func testUnauthorizedAndLimitsOnWire() throws {
        let (_, port) = try start()
        for (bytes, status) in [
            (request("/diagnostic/download?bytes=1", authenticated: false), 407),
            (request("example.com:22", method: "CONNECT"), 403),
            (request("/diagnostic/download?bytes=268435457"), 400),
            (Data(repeating: 65, count: HeaderParser.maximumBytes + 1), 431)
        ] {
            let fd = try openSocket(port: port)
            defer { Darwin.close(fd) }
            try writeAll(fd, bytes)
            XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 \(status)"))
            XCTAssertTrue(try readUntilEOF(fd).isEmpty)
        }
    }

    func testProductionCellularPolicyRejectsLoopbackPath() throws {
        let queue = DispatchQueue(label: "path-test")
        let ready = expectation(description: "path available")
        let listener = try NWListener(using: .tcp, on: .any)
        listener.newConnectionHandler = { connection in connection.start(queue: queue) }
        listener.stateUpdateHandler = { state in if case .ready = state { ready.fulfill() } }
        listener.start(queue: queue)
        defer { listener.cancel() }
        wait(for: [ready], timeout: 5)
        let pathReady = expectation(description: "connection path")
        let connection = NWConnection(host: "127.0.0.1", port: try XCTUnwrap(listener.port), using: .tcp)
        connection.stateUpdateHandler = { state in
            if case .ready = state {
                XCTAssertFalse(EgressPolicy.cellular.permits(connection.currentPath!))
                pathReady.fulfill()
            }
        }
        connection.start(queue: queue)
        defer { connection.stateUpdateHandler = nil; connection.cancel() }
        wait(for: [pathReady], timeout: 5)
    }

    func testCONNECTActualTCPBidirectionalPayloadAndHalfClose() throws {
        let echo = try EchoPeer()
        defer { echo.stop() }
        let policy = EgressPolicy(connection: { _ in
            NWConnection(host: "127.0.0.1", port: echo.port, using: .tcp)
        }, permits: { $0.status == .satisfied && $0.usesInterfaceType(.loopback) },
           peerAllowed: { _ in true })
        let (_, port) = try start(egress: policy)
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        let initial = Data([0, 255, 13, 10, 42])
        try writeAll(fd, request("example.com:443", method: "CONNECT") + initial)
        XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 200 Connection Established"))
        let payload = Data((0..<(2 * 1024 * 1024 + 17)).map { UInt8($0 % 239) })
        let sent = expectation(description: "upload and half close")
        DispatchQueue.global().async {
            do { try writeAll(fd, payload); Darwin.shutdown(fd, SHUT_WR) }
            catch { XCTFail("\(error)") }
            sent.fulfill()
        }
        XCTAssertEqual(try readUntilEOF(fd), initial + payload)
        wait(for: [sent], timeout: 10)
    }

    func testCONNECTPort80ActualTCP() throws {
        let echo = try EchoPeer()
        defer { echo.stop() }
        let policy = EgressPolicy(connection: { authority in
            XCTAssertEqual(authority.port, 80)
            return NWConnection(host: "127.0.0.1", port: echo.port, using: .tcp)
        }, permits: { $0.status == .satisfied && $0.usesInterfaceType(.loopback) },
           peerAllowed: { _ in true })
        let (_, port) = try start(egress: policy)
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        let body = Data("GET /depot/chunk HTTP/1.1\r\nHost: cdn.example\r\n\r\n".utf8)
        try writeAll(fd, request("cdn.example:80", method: "CONNECT") + body)
        XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 200 Connection Established"))
        Darwin.shutdown(fd, SHUT_WR)
        XCTAssertEqual(try readUntilEOF(fd), body)
    }

    func testThirtyTwoSessionsAndExcessConnectionRejection() throws {
        let accepted = expectation(description: "32 accepted sessions")
        accepted.expectedFulfillmentCount = ProxyServer.maximumSessions
        let echo = try EchoPeer()
        defer { echo.stop() }
        let policy = EgressPolicy(connection: { _ in
            NWConnection(host: "127.0.0.1", port: echo.port, using: .tcp)
        }, permits: { $0.status == .satisfied && $0.usesInterfaceType(.loopback) },
           peerAllowed: { _ in true })
        let (server, port) = try start(egress: policy, opened: accepted)
        var sockets: [Int32] = []
        defer { sockets.forEach { Darwin.close($0) } }
        for index in 0..<32 {
            let fd = try openSocket(port: port)
            sockets.append(fd)
            let targetPort = index % 2 == 0 ? 80 : 443
            try writeAll(fd, request("cdn.example:\(targetPort)", method: "CONNECT") + Data([UInt8(index)]))
            XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 200 Connection Established"))
            var echoed: UInt8 = 255
            XCTAssertEqual(Darwin.recv(fd, &echoed, 1, 0), 1)
            XCTAssertEqual(echoed, UInt8(index))
        }
        wait(for: [accepted], timeout: 5)
        let excess = try openSocket(port: port)
        defer { Darwin.close(excess) }
        var byte: UInt8 = 0
        let result = Darwin.recv(excess, &byte, 1, 0)
        XCTAssertTrue(result == 0 || (result < 0 && errno == ECONNRESET))
        server.stop()
        for fd in sockets {
            let count = Darwin.recv(fd, &byte, 1, 0)
            XCTAssertTrue(count == 0 || (count < 0 && errno == ECONNRESET))
        }
    }

    func testDeniedEgressNeverReturnsCONNECT200() throws {
        let echo = try EchoPeer()
        defer { echo.stop() }
        let policy = EgressPolicy(connection: { _ in
            NWConnection(host: "127.0.0.1", port: echo.port, using: .tcp)
        }, permits: { _ in false }, peerAllowed: { _ in true })
        let (_, port) = try start(egress: policy)
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        try writeAll(fd, request("example.com:443", method: "CONNECT"))
        XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 502"))
        XCTAssertTrue(try readUntilEOF(fd).isEmpty)
    }

    // MARK: R4 peer policy and protocol version

    private final class Recorder { var endpoints: [NWEndpoint?] = []; var connections = 0 }

    private func loopbackPolicy(echo: EchoPeer, recorder: Recorder = Recorder(),
                                peerAllowed: @escaping (NWEndpoint?) -> Bool) -> EgressPolicy {
        EgressPolicy(connection: { _ in
            recorder.connections += 1
            return NWConnection(host: "127.0.0.1", port: echo.port, using: .tcp)
        }, permits: { $0.status == .satisfied && $0.usesInterfaceType(.loopback) },
           peerAllowed: { recorder.endpoints.append($0); return peerAllowed($0) })
    }

    func testNonPublicLiteralRejectedBeforeAnyConnection() throws {
        let recorder = Recorder()
        let policy = EgressPolicy(connection: { _ in
            recorder.connections += 1
            XCTFail("a non-public literal must not create a connection")
            return NWConnection(host: "127.0.0.1", port: 9, using: .tcp)
        }, permits: { _ in true }, peerAllowed: { _ in true })
        let (_, port) = try start(egress: policy)
        for target in ["127.0.0.1:443", "10.0.0.1:80", "169.254.169.254:80", "192.168.1.1:443",
                       "100.64.0.1:443", "0.0.0.0:80", "[::1]:443", "[fe80::1]:443", "[fc00::1]:80",
                       "[::ffff:127.0.0.1]:443", "[64:ff9b::7f00:1]:443", "[2001:db8::1]:443"] {
            let fd = try openSocket(port: port)
            defer { Darwin.close(fd) }
            try writeAll(fd, request(target, method: "CONNECT"))
            XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 403"), target)
            XCTAssertTrue(try readUntilEOF(fd).isEmpty, target)
        }
        XCTAssertEqual(recorder.connections, 0)
    }

    func testLegacyNumericHostsRejected() throws {
        let recorder = Recorder()
        let policy = EgressPolicy(connection: { _ in
            recorder.connections += 1
            XCTFail("a legacy numeric host must not create a connection")
            return NWConnection(host: "127.0.0.1", port: 9, using: .tcp)
        }, permits: { _ in true }, peerAllowed: { _ in true })
        let (_, port) = try start(egress: policy)
        let hosts = ["127.1", "2130706433", "0x7f.0.0.1", "0x7f000001", "127.0.1", "010.0.0.1", "1.2.3.04",
                     "08.8.8.8", "300.1.1.1", "1.2.3.4.5", "7f.0.0.0x1"]
        for host in hosts {
            XCTAssertThrowsError(try Authority("\(host):443"), host) {
                XCTAssertEqual($0 as? ProtocolFailure, .malformed, host)
            }
            let fd = try openSocket(port: port)
            defer { Darwin.close(fd) }
            try writeAll(fd, request("\(host):443", method: "CONNECT"))
            XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 400"), host)
        }
        XCTAssertEqual(recorder.connections, 0)
        // Names that merely contain digits stay valid.
        XCTAssertNoThrow(try Authority("cdn1.example.com:443"))
        XCTAssertNoThrow(try Authority("1.example.com:443"))
    }

    func testNonPublicPeerGets403AndNoBytesReachPeer() throws {
        let echo = try EchoPeer()
        defer { echo.stop() }
        let recorder = Recorder()
        // Production peer policy against a loopback peer: the actual connected peer is non-public.
        let (_, port) = try start(egress: loopbackPolicy(echo: echo, recorder: recorder,
                                                         peerAllowed: PublicAddress.allows))
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        try writeAll(fd, request("cdn.example.com:443", method: "CONNECT") + Data("must-not-be-forwarded".utf8))
        let header = try readHeader(fd)
        XCTAssertTrue(header.hasPrefix("HTTP/1.1 403"), header)
        XCTAssertFalse(header.contains("200"))
        XCTAssertTrue(try readUntilEOF(fd).isEmpty)
        XCTAssertEqual(recorder.endpoints.count, 1)
        XCTAssertEqual(echo.receivedBytes, 0)
    }

    func testNilOrNamedPeerFailsClosed() throws {
        XCTAssertFalse(PublicAddress.allows(nil))
        XCTAssertFalse(PublicAddress.allows(.hostPort(host: .name("cdn.example.com", nil), port: 443)))
        XCTAssertFalse(PublicAddress.allows(.service(name: "n", type: "_t._tcp", domain: "local", interface: nil)))
        XCTAssertFalse(PublicAddress.allows(.unix(path: "/tmp/x")))
        XCTAssertTrue(PublicAddress.allows(.hostPort(host: .ipv4(try XCTUnwrap(IPv4Address("8.8.8.8"))), port: 443)))
        XCTAssertTrue(PublicAddress.allows(.hostPort(host: .ipv6(try XCTUnwrap(IPv6Address("2606:4700:4700::1111"))), port: 443)))
        XCTAssertFalse(PublicAddress.allows(.hostPort(host: .ipv4(try XCTUnwrap(IPv4Address("127.0.0.1"))), port: 443)))
        XCTAssertFalse(PublicAddress.allows(.hostPort(host: .ipv6(try XCTUnwrap(IPv6Address("::ffff:10.0.0.1"))), port: 443)))

        // On the wire: the session must hand the policy the unmodified endpoint and fail closed on its verdict.
        let echo = try EchoPeer()
        defer { echo.stop() }
        let (_, port) = try start(egress: loopbackPolicy(echo: echo) { _ in PublicAddress.allows(nil) })
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        try writeAll(fd, request("cdn.example.com:443", method: "CONNECT"))
        XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 403"))
        XCTAssertTrue(try readUntilEOF(fd).isEmpty)
        XCTAssertEqual(echo.receivedBytes, 0)
    }

    func testHostnameConnectReturns200WhenPeerAllowed() throws {
        let echo = try EchoPeer()
        defer { echo.stop() }
        let recorder = Recorder()
        let (_, port) = try start(egress: loopbackPolicy(echo: echo, recorder: recorder) { _ in true })
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        let payload = Data("hello-through-allowed-peer".utf8)
        try writeAll(fd, request("cdn.example.com:443", method: "CONNECT") + payload)
        XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 200 Connection Established"))
        Darwin.shutdown(fd, SHUT_WR)
        XCTAssertEqual(try readUntilEOF(fd), payload)
        // The policy saw the real numeric peer of the established socket, not the requested name.
        guard case .hostPort(let host, let peerPort)? = recorder.endpoints.first else {
            return XCTFail("peerAllowed did not receive a host/port endpoint: \(String(describing: recorder.endpoints))")
        }
        XCTAssertEqual(peerPort, echo.port)
        if case .ipv4(let address) = host { XCTAssertEqual(address, IPv4Address.loopback) }
        else { XCTFail("peer endpoint was not numeric: \(host)") }
    }

    func testPublicLiteralReachesConnectionAndPeerCheckStillRuns() throws {
        let echo = try EchoPeer()
        defer { echo.stop() }
        let recorder = Recorder()
        let (_, port) = try start(egress: loopbackPolicy(echo: echo, recorder: recorder) { _ in false })
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        try writeAll(fd, request("8.8.8.8:443", method: "CONNECT") + Data("x".utf8))
        XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 403"))
        XCTAssertEqual(recorder.connections, 1)
        XCTAssertEqual(echo.receivedBytes, 0)
    }

    func testProductionPeerPolicyRejectsLoopbackRemoteEndpoint() throws {
        let queue = DispatchQueue(label: "peer-policy-test")
        let ready = expectation(description: "listener ready")
        let listener = try NWListener(using: .tcp, on: .any)
        listener.newConnectionHandler = { connection in connection.start(queue: queue) }
        listener.stateUpdateHandler = { state in if case .ready = state { ready.fulfill() } }
        listener.start(queue: queue)
        defer { listener.cancel() }
        wait(for: [ready], timeout: 5)
        let peerReady = expectation(description: "connection ready")
        let connection = NWConnection(host: "127.0.0.1", port: try XCTUnwrap(listener.port), using: .tcp)
        var verdict: Bool?
        var endpoint: NWEndpoint?
        connection.stateUpdateHandler = { state in
            if case .ready = state {
                endpoint = connection.currentPath?.remoteEndpoint
                verdict = EgressPolicy.cellular.peerAllowed(endpoint)
                peerReady.fulfill()
            }
        }
        connection.start(queue: queue)
        defer { connection.stateUpdateHandler = nil; connection.cancel() }
        wait(for: [peerReady], timeout: 5)
        XCTAssertNotNil(endpoint, "a ready loopback connection must report a remote endpoint")
        XCTAssertEqual(verdict, false)
        XCTAssertFalse(EgressPolicy.cellular.peerAllowed(nil))
    }

    func testDiagnosticInfoBodyAndAuth() throws {
        XCTAssertEqual(ProxyProtocol.version, 2)
        let (_, port) = try start()
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        try writeAll(fd, request("/diagnostic/info"))
        let header = try readHeader(fd)
        XCTAssertTrue(header.hasPrefix("HTTP/1.1 200 OK"), header)
        XCTAssertTrue(header.contains("Content-Length: 14\r\n"), header)
        XCTAssertTrue(header.contains("Content-Type: application/json\r\n"), header)
        XCTAssertEqual(String(decoding: try readUntilEOF(fd), as: UTF8.self), "{\"protocol\":2}")

        let unauthenticated = try openSocket(port: port)
        defer { Darwin.close(unauthenticated) }
        try writeAll(unauthenticated, request("/diagnostic/info", authenticated: false))
        let rejected = try readHeader(unauthenticated)
        XCTAssertTrue(rejected.hasPrefix("HTTP/1.1 407"), rejected)
        XCTAssertTrue(try readUntilEOF(unauthenticated).isEmpty)

        let wrongMethod = try openSocket(port: port)
        defer { Darwin.close(wrongMethod) }
        try writeAll(wrongMethod, request("/diagnostic/info", method: "POST"))
        XCTAssertTrue(try readHeader(wrongMethod).hasPrefix("HTTP/1.1 405"))
    }

    func testStopRacingAcceptanceClosesSocket() throws {
        for _ in 0..<10 {
            let (server, port) = try start()
            let fd = try openSocket(port: port)
            defer { Darwin.close(fd) }
            try writeAll(fd, Data("CONNE".utf8))
            // Deliberately do not wait for the accept event: stop must reject queued accepts too.
            server.stop()
            var byte: UInt8 = 0
            let result = Darwin.recv(fd, &byte, 1, 0)
            XCTAssertTrue(result == 0 || (result < 0 && errno == ECONNRESET), "stop race: errno=\(errno)")
        }
    }

    func testStopCancelsIncompleteRequest() throws {
        let accepted = expectation(description: "accepted before cancellation")
        let (server, port) = try start(opened: accepted)
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        try writeAll(fd, Data("CONNE".utf8))
        wait(for: [accepted], timeout: 5)
        server.stop()
        // FIN or reset are both acceptable on cancellation; a read timeout is not.
        var byte: UInt8 = 0
        let count = Darwin.recv(fd, &byte, 1, 0)
        XCTAssertTrue(count == 0 || (count < 0 && errno == ECONNRESET))
    }
}

/// Bounded loopback echo fixture; no Internet, cellular, TLS or iOS behavior is implied.
private final class EchoPeer {
    let listener: NWListener
    let queue = DispatchQueue(label: "echo")
    var connections: [NWConnection] = []
    private var receivedCount = 0
    var port: NWEndpoint.Port { listener.port! }
    /// Application bytes this peer has read from any connection.
    var receivedBytes: Int { queue.sync { receivedCount } }

    init() throws {
        listener = try NWListener(using: .tcp, on: .any)
        let semaphore = DispatchSemaphore(value: 0)
        listener.stateUpdateHandler = { state in if case .ready = state { semaphore.signal() } }
        listener.newConnectionHandler = { [weak self] connection in
            guard let self else { connection.cancel(); return }
            self.connections.append(connection)
            connection.stateUpdateHandler = { [weak self, weak connection] state in
                if case .ready = state, let connection { self?.read(connection) }
            }
            connection.start(queue: self.queue)
        }
        listener.start(queue: queue)
        guard semaphore.wait(timeout: .now() + 5) == .success else { throw SocketFailure.syscall("echo ready timeout") }
    }

    func read(_ connection: NWConnection) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 16384) { [weak self] data, _, eof, error in
            guard error == nil else { connection.cancel(); return }
            self?.receivedCount += data?.count ?? 0
            connection.send(content: data, contentContext: eof ? .finalMessage : .defaultMessage,
                            isComplete: true, completion: .contentProcessed { error in
                if error != nil { connection.cancel() }
                else if !eof { self?.read(connection) }
                // Keep the FIN-sent fixture connection alive until the client has drained it.
                // stop() owns final cleanup; a send completion is not a remote-read acknowledgement.
            })
        }
    }

    func stop() {
        queue.sync {
            listener.cancel()
            connections.forEach { $0.stateUpdateHandler = nil; $0.cancel() }
            connections.removeAll()
        }
    }
}
