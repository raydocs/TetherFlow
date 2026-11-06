import XCTest
import Network
@testable import ProxyCore

final class PublicAddressTests: XCTestCase {
    private struct Vector { let line: Int; let address: String; let go: Bool; let swift: Bool }

    private func vectors() throws -> [Vector] {
        // Shared with Go (desktop/internal/steamaccel/security_vectors_test.go).
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("integration/public-ip-vectors.tsv")
        let text = try String(contentsOf: url, encoding: .utf8)
        var rows: [Vector] = []
        var sawHeader = false
        for (offset, line) in text.components(separatedBy: "\n").enumerated() {
            if line.trimmingCharacters(in: .whitespaces).isEmpty || line.hasPrefix("#") { continue }
            let columns = line.components(separatedBy: "\t")
            XCTAssertEqual(columns.count, 3, "line \(offset + 1)")
            guard columns.count == 3 else { continue }
            if !sawHeader {
                XCTAssertEqual(columns, ["address", "go", "swift"])
                sawHeader = true
                continue
            }
            func decide(_ value: String) -> Bool {
                XCTAssertTrue(value == "allow" || value == "deny", "line \(offset + 1): \(value)")
                return value == "allow"
            }
            rows.append(Vector(line: offset + 1, address: columns[0], go: decide(columns[1]), swift: decide(columns[2])))
        }
        return rows
    }

    func testPublicAddressVectors() throws {
        let rows = try vectors()
        XCTAssertGreaterThan(rows.count, 100)
        for row in rows {
            XCTAssertEqual(PublicAddress.allows(literal: row.address), row.swift,
                           "line \(row.line): \(row.address)")
        }
        // The only Go/Swift divergence is the documented NAT64 exception.
        let diverging = rows.filter { $0.go != $0.swift }
        XCTAssertFalse(diverging.isEmpty)
        for row in diverging {
            XCTAssertTrue(!row.go && row.swift && row.address.hasPrefix("64:ff9b::"), row.address)
        }
    }

    func testVectorsAlsoHoldForTypedEndpoints() throws {
        for row in try vectors() {
            let literal = String(row.address.split(separator: "%")[0])
            let host: NWEndpoint.Host
            if let v4 = IPv4Address(literal) { host = .ipv4(v4) }
            else if let v6 = IPv6Address(literal) { host = .ipv6(v6) }
            else { continue }
            XCTAssertEqual(PublicAddress.allows(.hostPort(host: host, port: 443)), row.swift,
                           "line \(row.line): \(row.address)")
        }
    }
}
