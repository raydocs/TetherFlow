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
        }, permits: { $0.status == .satisfied && $0.usesInterfaceType(.loopback) })
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
        }, permits: { $0.status == .satisfied && $0.usesInterfaceType(.loopback) })
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
        }, permits: { $0.status == .satisfied && $0.usesInterfaceType(.loopback) })
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
        }, permits: { _ in false })
        let (_, port) = try start(egress: policy)
        let fd = try openSocket(port: port)
        defer { Darwin.close(fd) }
        try writeAll(fd, request("example.com:443", method: "CONNECT"))
        XCTAssertTrue(try readHeader(fd).hasPrefix("HTTP/1.1 502"))
        XCTAssertTrue(try readUntilEOF(fd).isEmpty)
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
    var port: NWEndpoint.Port { listener.port! }

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
