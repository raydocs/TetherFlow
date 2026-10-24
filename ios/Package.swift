// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "TetherFlowIOS",
    platforms: [.macOS(.v13), .iOS(.v17)],
    products: [.library(name: "ProxyCore", targets: ["ProxyCore"])],
    targets: [
        .target(name: "ProxyCore"),
        .testTarget(name: "ProxyCoreTests", dependencies: ["ProxyCore"])
    ]
)
