package relay

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// HandleGatewayRegister handles WebSocket connections from gateways.
// The gateway sends a registration message with its token, and receives
// a gatewayId + connectCode in response.
func (h *Hub) HandleGatewayRegister(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("gateway upgrade failed: %v", err)
		return
	}
	ws := newWsConn(conn)

	token := r.URL.Query().Get("token")
	gw, code, err := h.registerGateway(ws, token)
	if err != nil {
		sendControlMessage(ws, "error", map[string]string{"error": err.Error()})
		ws.close()
		return
	}

	// send registration ack
	sendControlMessage(ws, "registered", map[string]string{
		"gatewayId":   gw.ID,
		"connectCode": code,
	})

	done := make(chan struct{})
	defer close(done)
	go startPinger(ws, done)

	ws.conn.SetReadLimit(1 << 20) // 1 MiB max message size
	ws.conn.SetReadDeadline(timeNow().Add(pongWait))
	ws.setPongHandler(func(string) error {
		ws.setReadDeadline(timeNow().Add(pongWait))
		return nil
	})

	// read loop: only control messages (tunnels handle data forwarding)
	for {
		_, raw, err := ws.readMessage()
		if err != nil {
			log.Printf("gateway read error (gw=%s): %v", gw.ID, err)
			break
		}
		h.handleGatewayControl(gw.ID, raw)
	}

	h.unregisterGateway(gw.ID)
}

// HandleClientConnect handles WebSocket connections from clients using a connect code.
// It pairs the client, then requests a tunnel from the gateway and bridges the two WSs.
func (h *Hub) HandleClientConnect(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("client upgrade failed: %v", err)
		return
	}
	ws := newWsConn(conn)

	cs, gwID, err := h.pairClient(ws, code)
	if err != nil {
		sendControlMessage(ws, "error", map[string]string{"error": err.Error()})
		ws.close()
		return
	}

	sendControlMessage(ws, "paired", map[string]string{
		"gatewayId":   gwID,
		"deviceToken": cs.DeviceToken,
	})

	// Request a tunnel from the gateway and bridge the client WS with it
	tunnelConn, err := h.requestTunnel(gwID)
	if err != nil {
		log.Printf("tunnel request failed for client connect (gw=%s): %v", gwID, err)
		sendControlMessage(ws, "error", map[string]string{"error": "tunnel failed"})
		ws.close()
		return
	}

	// Bridge blocks until one side disconnects
	bridgeWebSockets(ws.conn, tunnelConn)
	h.removeClient(cs.DeviceToken, cs.GatewayID)
}

// HandleClientReconnect handles WebSocket reconnections using gatewayId + deviceToken.
func (h *Hub) HandleClientReconnect(w http.ResponseWriter, r *http.Request) {
	gwID := r.URL.Query().Get("gw")
	deviceToken := r.URL.Query().Get("token")
	if gwID == "" || deviceToken == "" {
		http.Error(w, "missing gw or token parameter", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("client reconnect upgrade failed: %v", err)
		return
	}
	ws := newWsConn(conn)

	cs, err := h.reconnectClient(ws, gwID, deviceToken)
	if err != nil {
		sendControlMessage(ws, "error", map[string]string{"error": err.Error()})
		ws.close()
		return
	}

	tunnelConn, err := h.requestTunnel(gwID)
	if err != nil {
		log.Printf("tunnel request failed for client reconnect (gw=%s): %v", gwID, err)
		sendControlMessage(ws, "error", map[string]string{"error": "tunnel failed"})
		ws.close()
		return
	}

	bridgeWebSockets(ws.conn, tunnelConn)
	h.removeClient(cs.DeviceToken, cs.GatewayID)
}

// requestTunnel sends a tunnel_request to the gateway's control channel and waits
// for the gateway to connect back on /gateway/tunnel with the matching session ID.
func (h *Hub) requestTunnel(gwID string) (*websocket.Conn, error) {
	sessionID := generateID(16)
	ch := make(chan *websocket.Conn, 1)

	h.mu.Lock()
	gw, ok := h.gateways[gwID]
	if !ok {
		h.mu.Unlock()
		return nil, errGatewayGone
	}
	h.pendingTunnels[sessionID] = ch
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.pendingTunnels, sessionID)
		h.mu.Unlock()
	}()

	// Send tunnel_request on control channel
	if err := sendControlMessage(gw.Conn, "tunnel_request", map[string]string{
		"sessionId": sessionID,
	}); err != nil {
		return nil, err
	}

	// Wait for gateway to establish tunnel (timeout 15s)
	select {
	case tunnelConn := <-ch:
		return tunnelConn, nil
	case <-time.After(15 * time.Second):
		return nil, errors.New("tunnel request timed out")
	}
}

// renewConnectCode generates a fresh connect code for an existing gateway.
// Called via control message from gateway.
func (h *Hub) renewConnectCode(gwID string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.gateways[gwID]; !ok {
		return "", errGatewayGone
	}

	// remove old codes for this gateway
	for code, pc := range h.codes {
		if pc.GatewayID == gwID {
			delete(h.codes, code)
		}
	}

	code := generateConnectCode()
	h.codes[code] = &PendingCode{
		Code:      code,
		GatewayID: gwID,
		ExpiresAt: timeNow().Add(10 * time.Minute),
	}
	return code, nil
}

// timeNow is a variable for testing.
var timeNow = func() time.Time { return time.Now() }

// HandleGatewayTunnel handles a tunnel WebSocket from the gateway for a specific session.
// The gateway connects here after receiving a tunnel_request on the control channel.
// Once connected, this WS is bridged 1:1 with the waiting client WS.
func (h *Hub) HandleGatewayTunnel(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		http.Error(w, "missing session parameter", http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	ch, ok := h.pendingTunnels[sessionID]
	h.mu.RUnlock()
	if !ok {
		http.Error(w, "unknown or expired session", http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("tunnel upgrade failed: %v", err)
		return
	}

	// deliver the tunnel WS to the waiting client handler
	select {
	case ch <- conn:
		log.Printf("tunnel established: session=%s", sessionID)
	default:
		// channel already fulfilled or closed
		conn.Close()
	}
}

// bridgeWebSockets transparently bridges two WebSocket connections.
// All messages from ws1 are forwarded to ws2 and vice versa.
// When either side closes, the other is closed too.
func bridgeWebSockets(ws1, ws2 *websocket.Conn) {
	done := make(chan struct{}, 2)

	copy := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			msgType, data, err := src.ReadMessage()
			if err != nil {
				return
			}
			if err := dst.WriteMessage(msgType, data); err != nil {
				return
			}
		}
	}

	go copy(ws2, ws1)
	go copy(ws1, ws2)

	// wait for either direction to finish
	<-done
	ws1.Close()
	ws2.Close()
}

// handleGatewayControl processes control messages from gateway (non-relay-frame).
func (h *Hub) handleGatewayControl(gwID string, raw []byte) {
	var msg controlMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	switch msg.Type {
	case "renew_code":
		code, err := h.renewConnectCode(gwID)
		if err != nil {
			return
		}
		h.mu.RLock()
		gw, ok := h.gateways[gwID]
		h.mu.RUnlock()
		if ok {
			sendControlMessage(gw.Conn, "code_renewed", map[string]string{"connectCode": code})
		}
	}
}
