import Foundation
import Darwin

public enum ProtocolFailure: Error, Equatable {
    case headerTooLarge, malformed, unsupported, forbiddenPort, invalidByteCount
}

public struct Authority: Equatable {
    public let host: String
    public let port: UInt16

    public init(_ value: String) throws {
        let host: String
        let portText: Substring
        if value.hasPrefix("[") {
            guard let end = value.firstIndex(of: "]"),
                  value.index(after: end) < value.endIndex,
                  value[value.index(after: end)] == ":" else { throw ProtocolFailure.malformed }
            host = String(value[value.index(after: value.startIndex)..<end])
            portText = value[value.index(end, offsetBy: 2)...]
            var address = in6_addr()
            guard host.withCString({ inet_pton(AF_INET6, $0, &address) }) == 1 else {
                throw ProtocolFailure.malformed
            }
        } else {
            let parts = value.split(separator: ":", omittingEmptySubsequences: false)
            guard parts.count == 2 else { throw ProtocolFailure.malformed }
            host = String(parts[0])
            portText = parts[1]
            let labels = host.split(separator: ".", omittingEmptySubsequences: false)
            guard !host.isEmpty, host.utf8.count <= 253,
                  labels.allSatisfy({ label in
                      !label.isEmpty && label.count <= 63 && label.first != "-" && label.last != "-" &&
                      label.utf8.allSatisfy { (65...90).contains($0) || (97...122).contains($0) ||
                          (48...57).contains($0) || $0 == 45 }
                  }) else { throw ProtocolFailure.malformed }
        }
        guard !portText.isEmpty, portText.utf8.allSatisfy({ (48...57).contains($0) }),
              let port = UInt16(portText), port > 0 else { throw ProtocolFailure.malformed }
        self.host = host
        self.port = port
    }
}

public enum ProxyRoute: Equatable {
    case connect(Authority)
    case download(Int)
}

public struct ProxyRequest {
    public let method: String
    public let target: String
    public let headers: [String: String]

    public func authenticated(by token: String) -> Bool {
        let expected = Array(("Bearer " + token).utf8)
        let supplied = Array((headers["proxy-authorization"] ?? "").utf8)
        guard expected.count == supplied.count else { return false }
        var difference: UInt8 = 0
        for (a, b) in zip(expected, supplied) { difference |= a ^ b }
        return difference == 0
    }

    public func route() throws -> ProxyRoute {
        guard headers["transfer-encoding"] == nil,
              headers["content-length"] == nil || headers["content-length"] == "0" else {
            throw ProtocolFailure.malformed
        }
        if method == "CONNECT" {
            let authority = try Authority(target)
            guard authority.port == 80 || authority.port == 443 else { throw ProtocolFailure.forbiddenPort }
            return .connect(authority)
        }
        let prefix = "/diagnostic/download?bytes="
        if method == "GET", target.hasPrefix(prefix) {
            let text = target.dropFirst(prefix.count)
            guard !text.isEmpty, text.count <= 9, text.utf8.allSatisfy({ (48...57).contains($0) }),
                  let count = Int(text), (1...DiagnosticPayload.maximumBytes).contains(count) else {
                throw ProtocolFailure.invalidByteCount
            }
            return .download(count)
        }
        throw ProtocolFailure.unsupported
    }
}

/// One request per socket. Trailing bytes are opaque CONNECT data, never parsed as HTTP.
public struct HeaderParser {
    public static let maximumBytes = 16 * 1024
    public static let maximumInputChunk = 64 * 1024
    private var buffer = Data()
    private var completed = false
    public init() {}

    public mutating func append(_ data: Data) throws -> (ProxyRequest, Data)? {
        guard !completed, data.count <= Self.maximumInputChunk else { throw ProtocolFailure.malformed }
        buffer.append(data)
        guard let boundary = buffer.range(of: Data([13, 10, 13, 10])) else {
            guard buffer.count <= Self.maximumBytes else { throw ProtocolFailure.headerTooLarge }
            return nil
        }
        guard boundary.upperBound <= Self.maximumBytes else { throw ProtocolFailure.headerTooLarge }
        let header = buffer[..<boundary.lowerBound]
        guard header.allSatisfy({ $0 == 13 || $0 == 10 || (32...126).contains($0) }),
              let text = String(data: header, encoding: .ascii) else { throw ProtocolFailure.malformed }
        let lines = text.components(separatedBy: "\r\n")
        guard let first = lines.first else { throw ProtocolFailure.malformed }
        let requestLine = first.split(separator: " ", omittingEmptySubsequences: false)
        guard requestLine.count == 3, requestLine[2] == "HTTP/1.1",
              !requestLine[0].isEmpty, !requestLine[1].isEmpty else { throw ProtocolFailure.malformed }
        var headers: [String: String] = [:]
        for line in lines.dropFirst() {
            guard let colon = line.firstIndex(of: ":"), colon != line.startIndex else {
                throw ProtocolFailure.malformed
            }
            let name = String(line[..<colon]).lowercased()
            let tokens = "!#$%&'*+-.^_`|~0123456789abcdefghijklmnopqrstuvwxyz"
            guard name.allSatisfy({ tokens.contains($0) }), headers[name] == nil else {
                throw ProtocolFailure.malformed
            }
            let value = String(line[line.index(after: colon)...]).trimmingCharacters(in: .whitespaces)
            guard !value.contains("\r"), !value.contains("\n") else { throw ProtocolFailure.malformed }
            headers[name] = value
        }
        guard let host = headers["host"], !host.isEmpty else { throw ProtocolFailure.malformed }
        completed = true
        let trailing = Data(buffer[boundary.upperBound...])
        buffer.removeAll(keepingCapacity: false)
        return (ProxyRequest(method: String(requestLine[0]), target: String(requestLine[1]), headers: headers), trailing)
    }
}

/// A bounded reusable pattern; diagnostic bytes do not come from the Internet.
public enum DiagnosticPayload {
    public static let chunkBytes = 64 * 1024
    public static let maximumBytes = 256 * 1024 * 1024
    // A multiple of 251 preserves a continuous offset % 251 pattern on every send.
    public static let pattern = Data((0..<(251 * 256)).map { UInt8($0 % 251) })
}
