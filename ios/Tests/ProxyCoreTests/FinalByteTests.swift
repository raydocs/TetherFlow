import XCTest
import Network
import Darwin
@testable import ProxyCore

private enum FinalByteFailure: Error { case socket(String) }

/// BSD origin deliberately avoids NWConnection send/cancel lifetime in the echo fixture.
private final class HalfCloseOrigin {
    let listener: Int32
    let port: UInt16
    let tailReady = DispatchSemaphore(value: 0)
    let releaseTail = DispatchSemaphore(value: 0)
    let done = DispatchSemaphore(value: 0)
    var failure: Error?

    init(prefix: Data, tail: Data, waitForClientEOF: Bool) throws {
        let listener = Darwin.socket(AF_INET, SOCK_STREAM, 0)
        self.listener = listener
        guard listener >= 0 else { throw FinalByteFailure.socket("socket") }
        var address = sockaddr_in()
        address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        address.sin_family = sa_family_t(AF_INET)
        address.sin_addr.s_addr = inet_addr("127.0.0.1")
        let bound = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
        guard bound == 0, Darwin.listen(listener, 1) == 0 else {
            Darwin.close(listener)
            throw FinalByteFailure.socket("bind/listen")
        }
        var length = socklen_t(MemoryLayout<sockaddr_in>.size)
        let named = withUnsafeMutablePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.getsockname(listener, $0, &length) }
        }
        guard named == 0 else {
            Darwin.close(listener)
            throw FinalByteFailure.socket("getsockname")
        }
        port = UInt16(bigEndian: address.sin_port)
        DispatchQueue.global().async { [self] in
            defer { done.signal() }
            var pending = pollfd(fd: listener, events: Int16(POLLIN), revents: 0)
            guard Darwin.poll(&pending, 1, 8000) > 0 else {
                failure = FinalByteFailure.socket("accept deadline")
                return
            }
            let fd = Darwin.accept(listener, nil, nil)
            guard fd >= 0 else { failure = FinalByteFailure.socket("accept"); return }
            defer { Darwin.close(fd) }
            do {
                try configure(fd)
                // Let the complete bounded response enqueue while the consumer is gated;
                // the default send buffer can deadlock this deliberate backpressure setup.
                var sendBuffer: Int32 = 4 * 1024 * 1024
                guard setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &sendBuffer,
                                 socklen_t(MemoryLayout.size(ofValue: sendBuffer))) == 0 else {
                    throw FinalByteFailure.socket("origin send buffer errno=\(errno)")
                }
                // Proxy must propagate the client's write-half-close before the origin replies.
                if waitForClientEOF {
                    var byte: UInt8 = 0
                    guard Darwin.recv(fd, &byte, 1, 0) == 0 else { throw FinalByteFailure.socket("origin expected EOF") }
                }
                try sendAll(fd, prefix)
                tailReady.signal()
                guard releaseTail.wait(timeout: .now() + 8) == .success else { throw FinalByteFailure.socket("tail gate timeout") }
                try sendAll(fd, tail)
                guard Darwin.shutdown(fd, SHUT_WR) == 0 else { throw FinalByteFailure.socket("origin shutdown") }
                // BSD close after shutdown preserves queued TCP bytes; unlike an NW fixture,
                // this does not depend on a Network.framework completion or object lifetime.
            } catch { failure = error }
        }
    }

    func stop() {
        releaseTail.signal()
        Darwin.shutdown(listener, SHUT_RDWR)
        Darwin.close(listener)
        XCTAssertEqual(done.wait(timeout: .now() + 10), .success)
    }
}

private func configure(_ fd: Int32) throws {
    var timeout = timeval(tv_sec: 8, tv_usec: 0)
    var one: Int32 = 1
    guard setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout))) == 0,
          setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout))) == 0,
          setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout.size(ofValue: one))) == 0 else {
        throw FinalByteFailure.socket("setsockopt")
    }
}

private func sendAll(_ fd: Int32, _ data: Data) throws {
    try data.withUnsafeBytes { bytes in
        var offset = 0
        while offset < bytes.count {
            let count = Darwin.send(fd, bytes.baseAddress!.advanced(by: offset), bytes.count - offset, 0)
            guard count > 0 else { throw FinalByteFailure.socket("send errno=\(errno)") }
            offset += count
        }
    }
}

final class FinalByteTests: XCTestCase {
    func testControlledFinalFragmentAfterClientHalfClose() throws {
        try exerciseFinalFragment(clientHalfClose: true)
    }

    func testControlledFinalFragmentWithoutClientHalfClose() throws {
        try exerciseFinalFragment(clientHalfClose: false)
    }

    private func exerciseFinalFragment(clientHalfClose: Bool) throws {
        let prefix = Data((0..<(2 * 1024 * 1024)).map { UInt8($0 % 239) })
        let tail = Data((0..<17).map { UInt8($0 + 101) })
        let origin = try HalfCloseOrigin(prefix: prefix, tail: tail, waitForClientEOF: clientHalfClose)
        defer { origin.stop() }
        let ready = expectation(description: "proxy ready")
        var port: UInt16 = 0
        let closed = expectation(description: "session closed")
        let server = try ProxyServer(token: String(repeating: "c", count: 64), port: 0,
                                     egress: EgressPolicy(connection: { _ in
            NWConnection(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: origin.port)!, using: .tcp)
        }, permits: { $0.status == .satisfied && $0.usesInterfaceType(.loopback) })) { event in
            if case .listening(let bound) = event { port = bound; ready.fulfill() }
            if case .log(let message) = event, !message.hasPrefix("Session opened") {
                print("FINAL-BYTE \(message)")
                closed.fulfill()
            }
        }
        server.start()
        defer { server.stop() }
        wait(for: [ready], timeout: 5)
        let fd = Darwin.socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { throw FinalByteFailure.socket("client socket") }
        defer { Darwin.close(fd) }
        try configure(fd)
        var buffer: Int32 = 4096
        XCTAssertEqual(setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &buffer, socklen_t(MemoryLayout.size(ofValue: buffer))), 0)
        var address = sockaddr_in()
        address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        address.sin_family = sa_family_t(AF_INET)
        address.sin_port = port.bigEndian
        address.sin_addr.s_addr = inet_addr("127.0.0.1")
        let connected = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) }
        }
        XCTAssertEqual(connected, 0)
        try sendAll(fd, Data("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com\r\nProxy-Authorization: Bearer \(String(repeating: "c", count: 64))\r\n\r\n".utf8))
        if clientHalfClose { XCTAssertEqual(Darwin.shutdown(fd, SHUT_WR), 0) }
        var received = Data()
        let header = Data("HTTP/1.1 200 Connection Established\r\n\r\n".utf8)
        var chunk = [UInt8](repeating: 0, count: 1024)
        // Read only the CONNECT header; hold the body consumer until the origin
        // has enqueued its separate 17-byte tail and write-close.
        let gateCount = header.count
        while received.count < gateCount {
            let count = Darwin.recv(fd, &chunk, min(chunk.count, gateCount - received.count), 0)
            guard count > 0 else { throw FinalByteFailure.socket("prefix recv count=\(count) errno=\(errno)") }
            received.append(contentsOf: chunk.prefix(count))
        }
        XCTAssertEqual(origin.tailReady.wait(timeout: .now() + 5), .success)
        origin.releaseTail.signal()
        // Wait for the origin's final write/FIN, without a sleep or timing assumption.
        XCTAssertEqual(origin.done.wait(timeout: .now() + 5), .success)
        origin.done.signal()
        while true {
            let count = Darwin.recv(fd, &chunk, chunk.count, 0)
            if count == 0 { break }
            guard count > 0 else { throw FinalByteFailure.socket("tail recv errno=\(errno)") }
            guard received.count + count <= header.count + prefix.count + tail.count else {
                throw FinalByteFailure.socket("response exceeds bounded expected payload")
            }
            received.append(contentsOf: chunk.prefix(count))
        }
        XCTAssertEqual(received, header + prefix + tail)
        XCTAssertNil(origin.failure)
        if !clientHalfClose { XCTAssertEqual(Darwin.shutdown(fd, SHUT_WR), 0) }
        wait(for: [closed], timeout: 5)
    }
}
