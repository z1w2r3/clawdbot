import { IncomingMessage } from "node:http";
import { Socket } from "node:net";
import WebSocket from "ws";

type RelayLogger = {
  info: (msg: string) => void;
  warn: (msg: string) => void;
  error: (msg: string) => void;
};

/**
 * Relay control message from the relay server.
 */
type RelayControlMessage = {
  type: string;
  payload?: Record<string, string>;
};

export type RelayUplinkResult = {
  stop: () => Promise<void>;
  connectCode: string | null;
  gatewayId: string | null;
};

/**
 * Starts a persistent WebSocket uplink (control channel) to a relay server.
 *
 * The relay uses per-client tunnels: when a client connects to the relay,
 * the relay sends a `tunnel_request` on the control channel. The gateway
 * then opens a new WebSocket to the relay's /gateway/tunnel endpoint,
 * and the relay bridges the client WS and tunnel WS transparently.
 *
 * The tunnel WS is a real ws.WebSocket injected into the gateway's WSS
 * via `wss.emit("connection", tunnelWs, fakeReq)` — no virtual socket needed.
 */
export async function startGatewayRelayUplink(params: {
  relayUrl: string;
  relayToken: string;
  clients: Set<import("./server/ws-types.js").GatewayWsClient>;
  /** WebSocket server to inject tunnel connections into. */
  wss: import("ws").WebSocketServer;
  /** Called when a relay client sends a WS message to the gateway. */
  onClientMessage: (client: import("./server/ws-types.js").GatewayWsClient, data: string) => void;
  /** Called when a relay client connects (to set up the client in the gateway). */
  onClientConnected?: (client: import("./server/ws-types.js").GatewayWsClient) => void;
  /** Called when a relay client disconnects. */
  onClientDisconnected?: (client: import("./server/ws-types.js").GatewayWsClient) => void;
  log: RelayLogger;
}): Promise<RelayUplinkResult> {
  const { relayUrl, relayToken, log } = params;

  let ws: WebSocket | null = null;
  let stopped = false;
  let connectCode: string | null = null;
  let gatewayId: string | null = null;
  let reconnectAttempt = 0;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  function handleControlMessage(ctrl: RelayControlMessage) {
    switch (ctrl.type) {
      case "registered":
        gatewayId = ctrl.payload?.gatewayId ?? null;
        connectCode = ctrl.payload?.connectCode ?? null;
        log.info(`relay: registered (id=${gatewayId}, code=${connectCode})`);
        break;
      case "code_renewed":
        connectCode = ctrl.payload?.connectCode ?? null;
        log.info(`relay: connect code renewed: ${connectCode}`);
        break;
      case "tunnel_request": {
        const sessionId = ctrl.payload?.sessionId;
        if (!sessionId) {
          log.warn("relay: tunnel_request missing sessionId");
          break;
        }
        openTunnel(sessionId);
        break;
      }
      case "gateway_disconnected":
        log.warn("relay: server reports gateway disconnected");
        break;
      case "error":
        log.error(`relay: server error: ${ctrl.payload?.error ?? "unknown"}`);
        break;
    }
  }

  function openTunnel(sessionId: string) {
    const tunnelUrl = `${relayUrl}/gateway/tunnel?session=${encodeURIComponent(sessionId)}`;
    log.info(`relay: opening tunnel for session=${sessionId}`);

    let tunnelWs: WebSocket;
    try {
      tunnelWs = new WebSocket(tunnelUrl);
    } catch (err) {
      log.error(
        `relay: tunnel creation failed: ${err instanceof Error ? err.message : String(err)}`,
      );
      return;
    }

    tunnelWs.on("open", () => {
      log.info(`relay: tunnel connected for session=${sessionId}`);
      // Create a fake IncomingMessage so the gateway's ws-connection handler works
      const fakeSocket = new Socket();
      const fakeReq = new IncomingMessage(fakeSocket);
      fakeReq.headers = {
        host: "relay",
        "user-agent": "relay-tunnel",
        "x-forwarded-for": "relay",
      };
      fakeReq.url = "/";

      // Inject the real tunnel WebSocket into the gateway's WSS
      params.wss.emit("connection", tunnelWs, fakeReq);
    });

    tunnelWs.on("error", (err) => {
      log.error(`relay: tunnel error (session=${sessionId}): ${err.message}`);
    });
  }

  function handleRelayMessage(raw: string) {
    let parsed: RelayControlMessage;
    try {
      parsed = JSON.parse(raw);
    } catch {
      log.warn("relay: invalid JSON received");
      return;
    }

    if ("type" in parsed && typeof parsed.type === "string") {
      handleControlMessage(parsed);
    }
  }

  function connect() {
    if (stopped) return;

    const url = `${relayUrl}/gateway/register?token=${encodeURIComponent(relayToken)}`;
    log.info(`relay: connecting to ${relayUrl}...`);

    try {
      ws = new WebSocket(url);
    } catch (err) {
      log.error(
        `relay: connection creation failed: ${err instanceof Error ? err.message : String(err)}`,
      );
      scheduleReconnect();
      return;
    }

    ws.on("open", () => {
      reconnectAttempt = 0;
      log.info("relay: connected");
    });

    ws.on("message", (data) => {
      handleRelayMessage(data.toString());
    });

    ws.on("close", (code, reason) => {
      log.warn(`relay: disconnected (code=${code}, reason=${reason?.toString() ?? ""})`);
      if (!stopped) {
        scheduleReconnect();
      }
    });

    ws.on("error", (err) => {
      log.error(`relay: ws error: ${err.message}`);
    });

    ws.on("pong", () => {
      // keep-alive ack
    });
  }

  function scheduleReconnect() {
    if (stopped) return;
    reconnectAttempt++;
    // exponential backoff: 1s, 2s, 4s, 8s, ... max 60s
    const delay = Math.min(1000 * Math.pow(2, reconnectAttempt - 1), 60_000);
    log.info(`relay: reconnecting in ${Math.round(delay / 1000)}s (attempt ${reconnectAttempt})`);
    reconnectTimer = setTimeout(connect, delay);
  }

  async function stop() {
    stopped = true;
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    if (ws) {
      ws.close();
      ws = null;
    }
    log.info("relay: uplink stopped");
  }

  // Initial connection
  connect();

  // Wait briefly for initial registration
  await new Promise<void>((resolve) => {
    const check = () => {
      if (gatewayId || stopped) {
        resolve();
        return;
      }
      setTimeout(check, 100);
    };
    // timeout after 5s regardless
    setTimeout(resolve, 5000);
    check();
  });

  return { stop, connectCode, gatewayId };
}
