package com.clawdbot.android.gateway

data class GatewayEndpoint(
  val stableId: String,
  val name: String,
  val host: String,
  val port: Int,
  val lanHost: String? = null,
  val tailnetDns: String? = null,
  val gatewayPort: Int? = null,
  val canvasPort: Int? = null,
  val tlsEnabled: Boolean = false,
  val tlsFingerprintSha256: String? = null,
  /** Relay server URL for cloud-bridged connections (null for LAN/direct). */
  val relayUrl: String? = null,
  /** Gateway ID assigned by the relay server. */
  val relayGatewayId: String? = null,
  /** Device token for relay reconnection. */
  val relayDeviceToken: String? = null,
) {
  val isRelay: Boolean get() = relayUrl != null && relayGatewayId != null

  companion object {
    fun manual(host: String, port: Int): GatewayEndpoint =
      GatewayEndpoint(
        stableId = "manual|${host.lowercase()}|$port",
        name = "$host:$port",
        host = host,
        port = port,
        tlsEnabled = false,
        tlsFingerprintSha256 = null,
      )

    fun relay(relayUrl: String, gatewayId: String, deviceToken: String): GatewayEndpoint =
      GatewayEndpoint(
        stableId = "relay|$gatewayId",
        name = "Relay ($gatewayId)",
        host = relayUrl,
        port = 0,
        relayUrl = relayUrl,
        relayGatewayId = gatewayId,
        relayDeviceToken = deviceToken,
      )
  }
}
