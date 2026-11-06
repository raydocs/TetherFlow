import Foundation
import Darwin
import Network

public enum ProtocolFailure: Error, Equatable {
    case headerTooLarge, malformed, unsupported, forbiddenPort, invalidByteCount, nonPublicAddress
}

/// Wire protocol version reported by `GET /diagnostic/info`. 2 = the phone validates the connected peer.
public enum ProxyProtocol {
    public static let version = 2
    static var infoBody: Data { Data("{\"protocol\":\(version)}".utf8) }
}

/// Public-address policy for the phone path. Mirrors the Go `publicIP` deny list (see
/// `integration/public-ip-vectors.tsv`, the shared vectors) with one documented exception:
/// `64:ff9b::/96` (NAT64 on IPv6-only carriers) is allowed when its embedded IPv4 is public.
/// Carrier-specific NAT64 prefixes look like global IPv6 and cannot be checked; that is residual.
enum PublicAddress {
    private struct Prefix { let bytes: [UInt8]; let bits: Int }

    private static func prefixes(_ cidrs: [String]) -> [Prefix] {
        cidrs.map { cidr in
            let parts = cidr.split(separator: "/")
            guard parts.count == 2, let bits = Int(parts[1]), let bytes = parseLiteral(String(parts[0])) else {
                preconditionFailure("bad prefix \(cidr)")
            }
            return Prefix(bytes: bytes, bits: bits)
        }
    }

    private static let deniedV4 = prefixes([
        "0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
        "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
        "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3"])
    // 64:ff9b::/96 is handled before this list: allowed only with a public embedded IPv4.
    private static let deniedV6 = prefixes([
        "::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
        "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "ff00::/8"])
    private static let nat64 = prefixes(["64:ff9b::/96"])[0]
    private static let v4Mapped = prefixes(["::ffff:0:0/96"])[0]
    private static let globalUnicastV6 = prefixes(["2000::/3"])[0]

    private static func contains(_ prefix: Prefix, _ address: [UInt8]) -> Bool {
        guard prefix.bytes.count == address.count else { return false }
        for index in 0..<prefix.bytes.count {
            let remaining = prefix.bits - index * 8
            if remaining <= 0 { return true }
            let mask: UInt8 = remaining >= 8 ? 0xff : UInt8(truncatingIfNeeded: 0xff << (8 - remaining))
            if (prefix.bytes[index] ^ address[index]) & mask != 0 { return false }
        }
        return true
    }

    /// Strict dotted-quad IPv4 (no leading zeros, no legacy forms) or an IPv6 literal (zone id ignored).
    /// Returns nil for anything else, including legacy numeric hosts such as `127.1`.
    static func parseLiteral(_ text: String) -> [UInt8]? {
        if text.contains(":") {
            let bare = text.split(separator: "%", maxSplits: 1, omittingEmptySubsequences: false)[0]
            var address = in6_addr()
            guard bare.withCString({ inet_pton(AF_INET6, $0, &address) }) == 1 else { return nil }
            return withUnsafeBytes(of: &address) { Array($0) }
        }
        let parts = text.split(separator: ".", omittingEmptySubsequences: false)
        guard parts.count == 4 else { return nil }
        var bytes: [UInt8] = []
        for part in parts {
            guard (1...3).contains(part.utf8.count), part.utf8.allSatisfy({ (48...57).contains($0) }),
                  part.utf8.count == 1 || part.utf8.first != 48,
                  let value = UInt8(part) else { return nil }
            bytes.append(value)
        }
        return bytes
    }

    static func allows(literal: String) -> Bool {
        guard let bytes = parseLiteral(literal) else { return false }
        return allows(bytes: bytes)
    }

    static func allows(bytes: [UInt8]) -> Bool {
        if bytes.count == 4 { return !deniedV4.contains { contains($0, bytes) } }
        guard bytes.count == 16 else { return false }
        if contains(v4Mapped, bytes) || contains(nat64, bytes) { return allows(bytes: Array(bytes[12...])) }
        guard contains(globalUnicastV6, bytes) else { return false }
        return !deniedV6.contains { contains($0, bytes) }
    }

    /// The actual connected peer. Anything that is not a numeric host (nil, name, service, unix) fails closed.
    static func allows(_ endpoint: NWEndpoint?) -> Bool {
        guard case .hostPort(let host, _)? = endpoint else { return false }
        switch host {
        case .ipv4(let address): return allows(bytes: Array(address.rawValue))
        case .ipv6(let address): return allows(bytes: Array(address.rawValue))
        case .name: return false
        @unknown default: return false
        }
    }
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
            // A strict dotted quad is an IP literal (checked by route()). Any other host whose last
            // label is numeric (127.1, 2130706433, 0x7f.0.0.1) can be read as an address by downstream
            // stacks, so it is malformed, as in Go's validDNSName; so are inet_aton hex last labels.
            if PublicAddress.parseLiteral(host) == nil, let last = labels.last {
                let lowered = last.lowercased()
                let hex = lowered.hasPrefix("0x") && lowered.dropFirst(2).allSatisfy { $0.isHexDigit }
                if last.utf8.allSatisfy({ (48...57).contains($0) }) || hex { throw ProtocolFailure.malformed }
            }
        }
        guard !portText.isEmpty, portText.utf8.allSatisfy({ (48...57).contains($0) }),
              let port = UInt16(portText), port > 0 else { throw ProtocolFailure.malformed }
        self.host = host
        self.port = port
    }

    /// True for a numeric IPv4/IPv6 authority (no DNS involved).
    public var isIPLiteral: Bool { PublicAddress.parseLiteral(host) != nil }
}

public enum ProxyRoute: Equatable {
    case connect(Authority)
    case download(Int)
    case info
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
            // A non-public literal never reaches a connection; hostnames are checked on the real peer.
            if authority.isIPLiteral, !PublicAddress.allows(literal: authority.host) {
                throw ProtocolFailure.nonPublicAddress
            }
            return .connect(authority)
        }
        if method == "GET", target == "/diagnostic/info" { return .info }
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
