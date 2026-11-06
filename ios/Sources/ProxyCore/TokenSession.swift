import Foundation

/// Token text. 20 random bytes become 32 lowercase Crockford base32 characters (160 bits).
/// The desktop strips spaces and hyphens and lowercases input, so the grouped form can be typed as shown.
public enum TokenFormat {
    public static let byteCount = 20
    public static let length = 32
    /// Crockford base32, lowercase: no `i`, `l`, `o` or `u`.
    public static let alphabet = Array("0123456789abcdefghjkmnpqrstvwxyz".utf8)

    /// Encodes exactly `byteCount` bytes; returns nil for any other length.
    public static func generate(bytes: [UInt8]) -> String? {
        guard bytes.count == byteCount else { return nil }
        var output = [UInt8]()
        output.reserveCapacity(length)
        var buffer: UInt32 = 0
        var bits = 0
        for byte in bytes {
            buffer = (buffer << 8) | UInt32(byte)
            bits += 8
            while bits >= 5 {
                output.append(alphabet[Int((buffer >> UInt32(bits - 5)) & 31)])
                bits -= 5
            }
            buffer &= (1 << UInt32(bits)) - 1
        }
        return String(decoding: output, as: UTF8.self)
    }

    /// Display form: groups of four separated by single spaces. The copied/typed token is the same
    /// text without the separators.
    public static func grouped(_ token: String) -> String {
        var result = ""
        for (index, character) in token.enumerated() {
            if index > 0, index % 4 == 0 { result.append(" ") }
            result.append(character)
        }
        return result
    }
}

/// Token lifetime as a pure state machine. It owns no sockets, timers or storage: the app turns the
/// returned action into server calls. The token lives only in this value (process memory).
///
/// `now` must be a monotonic clock that keeps counting while the device sleeps. Production uses
/// `monotonicNanoseconds`; `DispatchTime.uptimeNanoseconds` stops during sleep and must not be used.
public struct TokenSession: CustomStringConvertible, CustomDebugStringConvertible {
    public enum State: Equatable { case stopped, running, paused }
    public enum Event: Equatable { case userStart, userStop, userRotate, scene(active: Bool), tick }
    public enum Action: Equatable { case startServer(String), stopServer, none }

    /// Longest time a paused token may stay in memory.
    public static let pauseWindowNanoseconds: UInt64 = 600 * 1_000_000_000

    /// CLOCK_MONOTONIC. The macOS/iOS SDK clock_gettime(3) documents it as incrementing while the
    /// system is asleep; CLOCK_UPTIME_RAW (= mach_absolute_time, = DispatchTime.uptimeNanoseconds) does not.
    public static let monotonicNanoseconds: () -> UInt64 = {
        clock_gettime_nsec_np(CLOCK_MONOTONIC)
    }

    public private(set) var state: State = .stopped
    public private(set) var token: String?
    /// Set when the last start or rotate could not produce a token (random source failed).
    public private(set) var generationFailed = false
    private var pausedAt: UInt64?

    private let generate: () -> String?
    private let now: () -> UInt64

    public init(generate: @escaping () -> String?, now: @escaping () -> UInt64) {
        self.generate = generate
        self.now = now
    }

    public var description: String { "TokenSession(\(state), token: <redacted>)" }
    public var debugDescription: String { description }

    public mutating func handle(_ event: Event) -> Action {
        expireIfNeeded()
        switch event {
        case .tick:
            return .none
        case .userStart:
            if state == .running { return .none }
            wipe()  // a start from .paused is an explicit new session: never reuse the paused token
            return issue()
        case .userStop:
            let wasRunning = state == .running
            wipe()
            return wasRunning ? .stopServer : .none
        case .userRotate:
            switch state {
            case .stopped: return .none
            case .running: return issue()
            case .paused:
                // New token now; the pause window is unchanged. It is served on resume.
                guard let fresh = generate() else { wipe(); generationFailed = true; return .none }
                generationFailed = false
                token = fresh
                return .none
            }
        case .scene(let active):
            if active {
                guard state == .paused, let token else { return .none }
                state = .running
                pausedAt = nil
                return .startServer(token)
            }
            guard state == .running else { return .none }
            state = .paused
            pausedAt = now()
            return .stopServer
        }
    }

    /// Nanoseconds until a paused token is destroyed; nil unless paused. For the app's timer only.
    public func pauseRemainingNanoseconds() -> UInt64? {
        guard state == .paused, let pausedAt else { return nil }
        let current = now()
        guard current >= pausedAt, current - pausedAt < Self.pauseWindowNanoseconds else { return 0 }
        return Self.pauseWindowNanoseconds - (current - pausedAt)
    }

    /// Wipes an expired paused token. Runs before every event, so an expired token can never resume.
    /// The window is [pause, pause + 10 min); a clock that went backwards fails closed.
    private mutating func expireIfNeeded() {
        if pauseRemainingNanoseconds() == 0 { wipe() }
    }

    private mutating func issue() -> Action {
        let wasRunning = state == .running
        guard let fresh = generate() else {
            wipe(); generationFailed = true  // fail closed: never keep serving the old token
            return wasRunning ? .stopServer : .none
        }
        generationFailed = false
        token = fresh
        state = .running
        pausedAt = nil
        return .startServer(fresh)
    }

    private mutating func wipe() {
        token = nil
        state = .stopped
        pausedAt = nil
        generationFailed = false
    }
}
