import XCTest
@testable import ProxyCore

final class TokenSessionTests: XCTestCase {
    private final class Fixture {
        var clock: UInt64 = 1_000
        var issued = 0
        var failNext = false
        func session() -> TokenSession {
            TokenSession(generate: { [self] in
                if failNext { failNext = false; return nil }
                issued += 1
                return "token-\(issued)-" + String(repeating: "x", count: 30)
            }, now: { [self] in clock })
        }
        func token(_ n: Int) -> String { "token-\(n)-" + String(repeating: "x", count: 30) }
    }
    private let minute: UInt64 = 60 * 1_000_000_000

    func testUserStartGeneratesFreshToken() {
        let f = Fixture()
        var s = f.session()
        XCTAssertEqual(s.handle(.userStart), .startServer(f.token(1)))
        XCTAssertEqual(s.state, .running)
        XCTAssertEqual(s.handle(.userStart), .none, "a second Start does not rotate")
        XCTAssertEqual(s.token, f.token(1))
        XCTAssertEqual(s.handle(.userStop), .stopServer)
        XCTAssertEqual(s.handle(.userStart), .startServer(f.token(2)), "Start after Stop is a new token")
    }

    func testSceneInactivePausesAndResumesSameTokenWithinWindow() {
        let f = Fixture()
        var s = f.session()
        _ = s.handle(.userStart)
        XCTAssertEqual(s.handle(.scene(active: false)), .stopServer)
        XCTAssertEqual(s.state, .paused)
        XCTAssertEqual(s.token, f.token(1), "kept in RAM while paused")
        XCTAssertEqual(s.handle(.scene(active: false)), .none, "repeated inactive/background is idempotent")
        f.clock += 10 * minute - 1
        XCTAssertEqual(s.handle(.scene(active: true)), .startServer(f.token(1)))
        XCTAssertEqual(s.state, .running)
        XCTAssertEqual(f.issued, 1)
        XCTAssertEqual(s.handle(.scene(active: true)), .none)
    }

    func testResumeAfterWindowWipesToken() {
        let f = Fixture()
        var s = f.session()
        _ = s.handle(.userStart)
        _ = s.handle(.scene(active: false))
        f.clock += 10 * minute
        XCTAssertEqual(s.handle(.scene(active: true)), .none)
        XCTAssertEqual(s.state, .stopped)
        XCTAssertNil(s.token)
        // Expiry is also applied by the timer tick, without any scene event.
        _ = s.handle(.userStart)
        _ = s.handle(.scene(active: false))
        XCTAssertEqual(s.pauseRemainingNanoseconds(), 10 * minute)
        f.clock += 10 * minute + 5
        XCTAssertEqual(s.pauseRemainingNanoseconds(), 0)
        XCTAssertEqual(s.handle(.tick), .none)
        XCTAssertNil(s.token)
        XCTAssertEqual(s.state, .stopped)
    }

    func testClockGoingBackwardsFailsClosed() {
        let f = Fixture()
        var s = f.session()
        _ = s.handle(.userStart)
        _ = s.handle(.scene(active: false))
        f.clock -= 1
        XCTAssertEqual(s.handle(.scene(active: true)), .none)
        XCTAssertNil(s.token)
    }

    func testUserStopWipesToken() {
        let f = Fixture()
        var s = f.session()
        _ = s.handle(.userStart)
        XCTAssertEqual(s.handle(.userStop), .stopServer)
        XCTAssertNil(s.token)
        XCTAssertEqual(s.state, .stopped)
        XCTAssertEqual(s.handle(.scene(active: true)), .none, "a stopped session never resumes")
        // Stop while paused: the server is already closed, the token is gone.
        _ = s.handle(.userStart)
        _ = s.handle(.scene(active: false))
        XCTAssertEqual(s.handle(.userStop), .none)
        XCTAssertNil(s.token)
        XCTAssertEqual(s.handle(.scene(active: true)), .none)
    }

    func testRotateIssuesNewToken() {
        let f = Fixture()
        var s = f.session()
        XCTAssertEqual(s.handle(.userRotate), .none, "nothing to rotate while stopped")
        _ = s.handle(.userStart)
        XCTAssertEqual(s.handle(.userRotate), .startServer(f.token(2)))
        XCTAssertEqual(s.token, f.token(2))
        // Rotating while paused changes the token that the resume serves, not the window.
        _ = s.handle(.scene(active: false))
        f.clock += minute
        XCTAssertEqual(s.handle(.userRotate), .none)
        XCTAssertEqual(s.pauseRemainingNanoseconds(), 9 * minute)
        XCTAssertEqual(s.handle(.scene(active: true)), .startServer(f.token(3)))
    }

    func testGeneratorFailureFailsClosed() {
        let f = Fixture()
        var s = f.session()
        f.failNext = true
        XCTAssertEqual(s.handle(.userStart), .none)
        XCTAssertEqual(s.state, .stopped)
        XCTAssertTrue(s.generationFailed)
        _ = s.handle(.userStart)
        f.failNext = true
        XCTAssertEqual(s.handle(.userRotate), .stopServer, "a failed rotate must not keep serving the old token")
        XCTAssertNil(s.token)
    }

    func testTokenFormatThirtyTwoCrockfordLowercase() {
        XCTAssertNil(TokenFormat.generate(bytes: [UInt8](repeating: 1, count: 19)))
        XCTAssertEqual(TokenFormat.generate(bytes: [UInt8](repeating: 0, count: 20)),
                       String(repeating: "0", count: 32))
        XCTAssertEqual(TokenFormat.generate(bytes: [UInt8](repeating: 0xff, count: 20)),
                       String(repeating: "z", count: 32))
        let allowed = Set("0123456789abcdefghjkmnpqrstvwxyz")
        XCTAssertEqual(allowed.count, 32)
        var seen = Set<Character>()
        for seed in 0..<64 {
            let bytes = (0..<20).map { UInt8(truncatingIfNeeded: seed &* 37 &+ $0 &* 11 &+ ($0 * $0)) }
            let token = try! XCTUnwrap(TokenFormat.generate(bytes: bytes))
            XCTAssertEqual(token.count, 32)
            XCTAssertTrue(token.allSatisfy(allowed.contains), token)
            XCTAssertTrue((32...128).contains(token.utf8.count))
            seen.formUnion(token)
        }
        XCTAssertTrue(seen.isSubset(of: allowed))
        // The first symbol is the top 5 bits of byte 0.
        XCTAssertEqual(TokenFormat.generate(bytes: [0b10101_000] + [UInt8](repeating: 0, count: 19))?.first, "n")  // 0b10101 = 21 -> "n"
        XCTAssertEqual(TokenFormat.grouped(String(repeating: "a", count: 32)),
                       Array(repeating: "aaaa", count: 8).joined(separator: " "))
        XCTAssertEqual(TokenFormat.grouped("abcde"), "abcd e")
    }

    func testProductionClockIsMonotonicAndDescriptionHidesToken() {
        let a = TokenSession.monotonicNanoseconds()
        let b = TokenSession.monotonicNanoseconds()
        XCTAssertGreaterThanOrEqual(b, a)
        XCTAssertGreaterThan(a, 0)
        let f = Fixture()
        var s = f.session()
        _ = s.handle(.userStart)
        XCTAssertFalse("\(s) \(String(reflecting: s))".contains("token-1"))
    }
}
