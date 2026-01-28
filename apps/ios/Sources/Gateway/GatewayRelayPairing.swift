import Foundation
import OSLog

private let logger = Logger(subsystem: "com.clawdbot", category: "relay.pairing")

/// Handles pairing with a gateway through the relay server using a connect code.
enum GatewayRelayPairing {
    /// Result of a successful relay pairing.
    struct PairingResult: Sendable {
        let gatewayId: String
        let deviceToken: String
        let relayUrl: String
    }

    /// Pairs with a gateway via the relay server using the given connect code.
    /// - Parameters:
    ///   - relayUrl: The relay server base URL (e.g. "wss://relay.clawd.bot").
    ///   - code: The connect code displayed by the gateway (e.g. "ABCD-1234").
    /// - Returns: A `PairingResult` on success.
    static func pair(relayUrl: String, code: String) async throws -> PairingResult {
        let urlString = "\(relayUrl)/client/connect?code=\(code)"
        guard let url = URL(string: urlString) else {
            throw RelayPairingError.invalidURL(urlString)
        }

        logger.info("relay pairing: connecting to \(urlString, privacy: .public)")

        let session = URLSession(configuration: .default)
        let task = session.webSocketTask(with: url)
        task.maximumMessageSize = 1024 * 1024
        task.resume()

        // Wait for the "paired" control message from the relay server
        let message = try await task.receive()
        guard case let .string(text) = message,
              let data = text.data(using: .utf8),
              let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let type = json["type"] as? String
        else {
            task.cancel(with: .normalClosure, reason: nil)
            throw RelayPairingError.unexpectedResponse
        }

        if type == "error" {
            let payload = json["payload"] as? [String: Any]
            let errorMsg = payload?["error"] as? String ?? "unknown error"
            task.cancel(with: .normalClosure, reason: nil)
            throw RelayPairingError.serverError(errorMsg)
        }

        guard type == "paired",
              let payload = json["payload"] as? [String: String],
              let gatewayId = payload["gatewayId"],
              let deviceToken = payload["deviceToken"]
        else {
            task.cancel(with: .normalClosure, reason: nil)
            throw RelayPairingError.unexpectedResponse
        }

        logger.info("relay pairing: paired successfully (gatewayId=\(gatewayId, privacy: .public))")

        // Close the pairing WebSocket; the app will reconnect using the reconnect endpoint
        task.cancel(with: .normalClosure, reason: nil)

        return PairingResult(
            gatewayId: gatewayId,
            deviceToken: deviceToken,
            relayUrl: relayUrl
        )
    }

    enum RelayPairingError: LocalizedError {
        case invalidURL(String)
        case unexpectedResponse
        case serverError(String)

        var errorDescription: String? {
            switch self {
            case .invalidURL(let url):
                return "Invalid relay URL: \(url)"
            case .unexpectedResponse:
                return "Unexpected response from relay server"
            case .serverError(let message):
                return "Relay server error: \(message)"
            }
        }
    }
}
