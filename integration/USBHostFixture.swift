// HOST-ONLY INTEGRATION FIXTURE. Never included in the iOS app or a release.
// Compiled alongside the production core to inject a loopback test origin.
import Foundation
import Network

@main
struct USBHostFixture {
    static func main() throws {
        guard CommandLine.arguments.count == 4,
              let originPort = UInt16(CommandLine.arguments[1]), originPort > 0,
              let tlsOriginPort = UInt16(CommandLine.arguments[2]), tlsOriginPort > 0 else {
            fatalError("fixture requires loopback HTTP/TLS origin ports and test-only token")
        }
        let egress = EgressPolicy(connection: { authority in
            NWConnection(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: authority.port == 443 ? tlsOriginPort : originPort)!, using: .tcp)
        }, permits: { $0.status == .satisfied })
        let server = try ProxyServer(token: CommandLine.arguments[3], port: 0, egress: egress) { event in
            if case .log(let message) = event { FileHandle.standardError.write(Data("SWIFT \(message)\n".utf8)) }
            if case .listening(let port) = event {
                FileHandle.standardOutput.write(Data("READY \(port)\n".utf8))
            }
        }
        server.start()
        withExtendedLifetime(server) { RunLoop.main.run() }
    }
}
