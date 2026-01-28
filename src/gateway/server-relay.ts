import { randomUUID } from "node:crypto";
import WebSocket from "ws";
import type { GatewayWsClient } from "./server/ws-types.js";

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

/**
 * Relay frame envelope: wraps a client message forwarded through the relay.
 */
type RelayFrame = {
  from: string; // "client:<deviceToken>"
  data: string; // raw message from client (JSON string)
};

export type RelayUplinkResult = {
  stop: () => Promise<void>;
  connectCode: string | null;
  gatewayId: string | null;
};

/**
 * Starts a persistent WebSocket uplink to a relay server.
 *
 * The relay acts as a transparent bridge: mobile clients connect to the relay
 * with a connect code, and the relay forwards their messages here. Responses
 * from the gateway are sent back through the relay to the client.
 *
 * Follows the same start/cleanup pattern as server-tailscale.ts.
 */
export async function startGatewayRelayUplink(params: {
  relayUrl: string;
  relayToken: string;
  clients: Set<GatewayWsClient>;
  /** Called when a relay client sends a WS message to the gateway. */
  onClientMessage: (client: GatewayWsClient, data: string) => void;
  /** Called when a relay client connects (to set up the client in the gateway). */
  onClientConnected?: (client: GatewayWsClient) => void;
  /** Called when a relay client disconnects. */
  onClientDisconnected?: (client: GatewayWsClient) => void;
  log: RelayLogger;
}): Promise<RelayUplinkResult> {
  const { relayUrl, relayToken, clients, onClientMessage, log } = params;

  let ws: WebSocket | null = null;
  let stopped = false;
  let connectCode: string | null = null;
  let gatewayId: string | null = null;
  let reconnectAttempt = 0;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  // Virtual clients connected through the relay, keyed by deviceToken.
  const relayClients = new Map<string, GatewayWsClient>();

  function createVirtualClient(deviceToken: string): GatewayWsClient {
    // Create a virtual WebSocket-like object that sends data back through the relay.
    const virtualSocket = new VirtualRelaySocket(deviceToken, () => ws);
    const client: GatewayWsClient = {
      socket: virtualSocket as unknown as import("ws").WebSocket,
      connect: {
        minProtocol: 1,
        maxProtocol: 1,
        client: {
          id: `relay-${deviceToken}`,
          version: "relay/1.0",
          platform: "relay",
          mode: "mobile",
        },
      } as unknown as GatewayWsClient["connect"],
      connId: randomUUID(),
    };
    return client;
  }

  function handleRelayMessage(raw: string) {
    let parsed: RelayControlMessage | RelayFrame;
    try {
      parsed = JSON.parse(raw);
    } catch {
      log.warn("relay: invalid JSON received");
      return;
    }

    // Control messages from relay server
    if ("type" in parsed && typeof (parsed as RelayControlMessage).type === "string") {
      const ctrl = parsed as RelayControlMessage;
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
        case "gateway_disconnected":
          log.warn("relay: server reports gateway disconnected");
          break;
        case "error":
          log.error(`relay: server error: ${ctrl.payload?.error ?? "unknown"}`);
          break;
        default:
          // unknown control message, ignore
          break;
      }
      return;
    }

    // Relay frame: forwarded client message
    if ("from" in parsed && "data" in parsed) {
      const frame = parsed as RelayFrame;
      const deviceToken = frame.from.startsWith("client:") ? frame.from.slice(7) : frame.from;

      let client = relayClients.get(deviceToken);
      if (!client) {
        // new relay client
        client = createVirtualClient(deviceToken);
        relayClients.set(deviceToken, client);
        clients.add(client);
        params.onClientConnected?.(client);
        log.info(`relay: client connected via relay (device=${deviceToken})`);
      }

      // Forward the raw message data to the gateway's message handler
      try {
        const dataStr = typeof frame.data === "string" ? frame.data : JSON.stringify(frame.data);
        onClientMessage(client, dataStr);
      } catch (err) {
        log.error(
          `relay: error handling client message: ${err instanceof Error ? err.message : String(err)}`,
        );
      }
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
      cleanupRelayClients();
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

  function cleanupRelayClients() {
    for (const [deviceToken, client] of relayClients) {
      clients.delete(client);
      params.onClientDisconnected?.(client);
      log.info(`relay: client disconnected (device=${deviceToken})`);
    }
    relayClients.clear();
  }

  async function stop() {
    stopped = true;
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    cleanupRelayClients();
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

/**
 * A virtual WebSocket that sends data back through the relay connection.
 * This is used to create GatewayWsClient instances for relay-connected clients.
 */
class VirtualRelaySocket {
  private deviceToken: string;
  private getWs: () => WebSocket | null;
  readyState: number = WebSocket.OPEN;

  // EventEmitter stubs so the gateway code doesn't crash
  // biome-ignore lint/suspicious/noExplicitAny: ws EventEmitter compat
  on(_event: string, _fn: (...args: any[]) => void) {
    return this;
  }
  // biome-ignore lint/suspicious/noExplicitAny: ws EventEmitter compat
  off(_event: string, _fn: (...args: any[]) => void) {
    return this;
  }
  // biome-ignore lint/suspicious/noExplicitAny: ws EventEmitter compat
  once(_event: string, _fn: (...args: any[]) => void) {
    return this;
  }
  // biome-ignore lint/suspicious/noExplicitAny: ws EventEmitter compat
  removeListener(_event: string, _fn: (...args: any[]) => void) {
    return this;
  }
  // biome-ignore lint/suspicious/noExplicitAny: ws EventEmitter compat
  addListener(_event: string, _fn: (...args: any[]) => void) {
    return this;
  }

  constructor(deviceToken: string, getWs: () => WebSocket | null) {
    this.deviceToken = deviceToken;
    this.getWs = getWs;
  }

  send(data: string | Buffer, cb?: (err?: Error) => void) {
    const relayWs = this.getWs();
    if (!relayWs || relayWs.readyState !== WebSocket.OPEN) {
      cb?.(new Error("relay connection not available"));
      return;
    }
    // Send as a relay frame targeting this client
    const frame = JSON.stringify({
      from: `client:${this.deviceToken}`,
      data: typeof data === "string" ? data : data.toString("utf8"),
    });
    relayWs.send(frame, cb);
  }

  close(_code?: number, _reason?: string) {
    this.readyState = WebSocket.CLOSED;
  }

  ping() {
    /* no-op for virtual sockets */
  }
  pong() {
    /* no-op for virtual sockets */
  }
  terminate() {
    this.close();
  }

  get bufferedAmount() {
    return 0;
  }
  get extensions() {
    return "";
  }
  get protocol() {
    return "";
  }
  get binaryType(): "nodebuffer" {
    return "nodebuffer";
  }
  set binaryType(_v: string) {
    /* no-op */
  }
}
