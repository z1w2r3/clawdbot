package com.clawdbot.android.gateway

import android.content.Context
import android.content.SharedPreferences
import android.util.Log
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONObject
import java.util.concurrent.TimeUnit
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

private const val TAG = "RelayPairing"
private const val PREFS_NAME = "relay_pairing"
private const val KEY_RELAY_URL = "relay_url"
private const val KEY_GATEWAY_ID = "gateway_id"
private const val KEY_DEVICE_TOKEN = "device_token"

data class RelayPairingResult(
  val gatewayId: String,
  val deviceToken: String,
  val relayUrl: String,
)

/** Pairs with a gateway through the relay server using a connect code. */
suspend fun relayPair(relayUrl: String, code: String): RelayPairingResult =
  withContext(Dispatchers.IO) {
    val client = OkHttpClient.Builder()
      .connectTimeout(10, TimeUnit.SECONDS)
      .readTimeout(30, TimeUnit.SECONDS)
      .build()

    val url = "$relayUrl/client/connect?code=$code"
    Log.i(TAG, "relay pairing: connecting to $url")

    val request = Request.Builder().url(url).build()

    suspendCancellableCoroutine { continuation ->
      val ws = client.newWebSocket(request, object : WebSocketListener() {
        override fun onMessage(webSocket: WebSocket, text: String) {
          try {
            val json = JSONObject(text)
            val type = json.optString("type")

            if (type == "error") {
              val payload = json.optJSONObject("payload")
              val errorMsg = payload?.optString("error") ?: "unknown error"
              webSocket.close(1000, "error")
              continuation.resumeWithException(
                RelayPairingException("Relay server error: $errorMsg")
              )
              return
            }

            if (type == "paired") {
              val payload = json.getJSONObject("payload")
              val gatewayId = payload.getString("gatewayId")
              val deviceToken = payload.getString("deviceToken")
              webSocket.close(1000, "paired")
              Log.i(TAG, "relay pairing: paired successfully (gatewayId=$gatewayId)")
              continuation.resume(
                RelayPairingResult(
                  gatewayId = gatewayId,
                  deviceToken = deviceToken,
                  relayUrl = relayUrl,
                )
              )
              return
            }

            Log.w(TAG, "relay pairing: unexpected message type: $type")
          } catch (e: Exception) {
            webSocket.close(1000, "error")
            continuation.resumeWithException(e)
          }
        }

        override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
          Log.e(TAG, "relay pairing: connection failed", t)
          continuation.resumeWithException(
            RelayPairingException("Relay connection failed: ${t.message}", t)
          )
        }
      })

      continuation.invokeOnCancellation {
        ws.close(1000, "cancelled")
      }
    }
  }

class RelayPairingException(message: String, cause: Throwable? = null) : Exception(message, cause)

/** Persists relay pairing credentials. */
object RelayPairingStore {
  private fun prefs(context: Context): SharedPreferences =
    context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)

  fun save(context: Context, result: RelayPairingResult) {
    prefs(context).edit()
      .putString(KEY_RELAY_URL, result.relayUrl)
      .putString(KEY_GATEWAY_ID, result.gatewayId)
      .putString(KEY_DEVICE_TOKEN, result.deviceToken)
      .apply()
  }

  fun loadRelayUrl(context: Context): String? = prefs(context).getString(KEY_RELAY_URL, null)
  fun loadGatewayId(context: Context): String? = prefs(context).getString(KEY_GATEWAY_ID, null)
  fun loadDeviceToken(context: Context): String? = prefs(context).getString(KEY_DEVICE_TOKEN, null)

  fun clear(context: Context) {
    prefs(context).edit().clear().apply()
  }

  fun hasPairing(context: Context): Boolean =
    loadGatewayId(context) != null && loadDeviceToken(context) != null
}
