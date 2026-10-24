import XCTest
@testable import ProxyCore

final class ProtocolTests: XCTestCase {
    let token = String(repeating: "a", count: 64)
    var connect: Data {
        Data("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: Bearer \(token)\r\n\r\n".utf8)
    }

    func parse(_ text: String) throws -> ProxyRequest {
        var parser = HeaderParser()
        let result = try parser.append(Data(text.utf8))
        return try XCTUnwrap(result).0
    }

    func testEverySplitBoundaryAndOpaqueTrailingData() throws {
        let payload = Data([0, 255, 13, 10, 0, 42])
        for split in 0..<connect.count {
            var parser = HeaderParser()
            XCTAssertNil(try parser.append(Data(connect.prefix(split))))
            let result = try XCTUnwrap(parser.append(Data(connect.dropFirst(split)) + payload))
            XCTAssertEqual(result.1, payload)
            XCTAssertEqual(try result.0.route(), .connect(try Authority("example.com:443")))
            XCTAssertTrue(result.0.authenticated(by: token))
            XCTAssertThrowsError(try parser.append(Data()))
        }
    }

    func testOneByteChunks() throws {
        var parser = HeaderParser()
        for byte in connect.dropLast() { XCTAssertNil(try parser.append(Data([byte]))) }
        XCTAssertNotNil(try parser.append(Data(connect.suffix(1))))
    }

    func testExactHeaderLimitAndOverLimitWithTerminator() throws {
        let base = "GET /diagnostic/download?bytes=1 HTTP/1.1\r\nHost: localhost\r\nX-Pad: "
        let pad = HeaderParser.maximumBytes - base.utf8.count - 4
        var parser = HeaderParser()
        XCTAssertNotNil(try parser.append(Data((base + String(repeating: "x", count: pad) + "\r\n\r\n").utf8)))
        parser = HeaderParser()
        XCTAssertThrowsError(try parser.append(Data((base + String(repeating: "x", count: pad + 1) + "\r\n\r\n").utf8))) {
            XCTAssertEqual($0 as? ProtocolFailure, .headerTooLarge)
        }
    }

    func testIncompleteHeaderLimitAndOversizedChunk() throws {
        var parser = HeaderParser()
        XCTAssertNil(try parser.append(Data(repeating: 65, count: HeaderParser.maximumBytes)))
        XCTAssertThrowsError(try parser.append(Data([65]))) { XCTAssertEqual($0 as? ProtocolFailure, .headerTooLarge) }
        parser = HeaderParser()
        XCTAssertThrowsError(try parser.append(Data(repeating: 65, count: HeaderParser.maximumInputChunk + 1)))
    }

    func testMalformedAndAmbiguousFramingRejected() {
        let cases = [
            "CONNECT example.com:443 HTTP/1.0\r\nHost: a\r\n\r\n",
            "CONNECT example.com:443 HTTP/1.1\r\n\r\n",
            "CONNECT example.com:443 HTTP/1.1\r\nHost: a\r\nhost: b\r\n\r\n",
            "CONNECT example.com:443 HTTP/1.1\r\nHost: a\r\n Fold: x\r\n\r\n",
            "CONNECT example.com:443 HTTP/1.1\r\nHost: a\r\nX: a\nb\r\n\r\n",
            "CONNECT example.com:443 HTTP/1.1\r\nHost: a\r\nX: \tbad\r\n\r\n"
        ]
        for text in cases { XCTAssertThrowsError(try parse(text), text) }
        for framing in ["Transfer-Encoding: chunked", "Content-Length: 1", "Content-Length: -1"] {
            XCTAssertThrowsError(try parse("CONNECT example.com:443 HTTP/1.1\r\nHost: a\r\n\(framing)\r\n\r\n").route())
        }
    }

    func testAuthorityAndPortRestrictions() throws {
        XCTAssertEqual(try Authority("[2001:db8::1]:443").host, "2001:db8::1")
        for invalid in ["a", "a:0", "a:65536", "a:+443", "user@host:443", "a/b:443", "a:443:443",
                        "[not-ipv6]:443", "::1:443", "[::1]443", "-a:443", "a..b:443"] {
            XCTAssertThrowsError(try Authority(invalid), invalid)
        }
        for port in [80, 443] {
            XCTAssertEqual(try parse("CONNECT example.com:\(port) HTTP/1.1\r\nHost: a\r\n\r\n").route(),
                           .connect(try Authority("example.com:\(port)")))
        }
        for port in [1, 22, 81, 444, 8080, 65535] {
            XCTAssertThrowsError(try parse("CONNECT example.com:\(port) HTTP/1.1\r\nHost: a\r\n\r\n").route()) {
                XCTAssertEqual($0 as? ProtocolFailure, .forbiddenPort)
            }
        }
    }

    func testAuthenticationAndDiagnosticBounds() throws {
        let request = try parse(String(decoding: connect, as: UTF8.self))
        XCTAssertFalse(request.authenticated(by: String(repeating: "b", count: 64)))
        XCTAssertFalse(try parse("CONNECT a:443 HTTP/1.1\r\nHost: a\r\n\r\n").authenticated(by: token))
        for count in [1, DiagnosticPayload.maximumBytes] {
            XCTAssertEqual(try parse("GET /diagnostic/download?bytes=\(count) HTTP/1.1\r\nHost: a\r\n\r\n").route(), .download(count))
        }
        for count in ["0", "-1", "268435457", "99999999999999999999", "1&x=2", "1.5", ""] {
            XCTAssertThrowsError(try parse("GET /diagnostic/download?bytes=\(count) HTTP/1.1\r\nHost: a\r\n\r\n").route())
        }
        XCTAssertThrowsError(try parse("GET /pac HTTP/1.1\r\nHost: a\r\n\r\n").route())
    }

    func testTokenAndChunkConfiguration() throws {
        XCTAssertThrowsError(try ProxyServer(token: "short", event: { _ in }))
        XCTAssertLessThanOrEqual(DiagnosticPayload.pattern.count, DiagnosticPayload.chunkBytes)
        XCTAssertEqual(DiagnosticPayload.pattern.count % 251, 0)
    }
}
