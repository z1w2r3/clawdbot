package relay

import (
	"encoding/json"
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

	ws.conn.SetReadDeadline(timeNow().Add(pongWait))
	ws.setPongHandler(func(string) error {
		ws.setReadDeadline(timeNow().Add(pongWait))
		return nil
	})

	// read loop: forward data from gateway to targeted client(s)
	for {
		_, raw, err := ws.readMessage()
		if err != nil {
			log.Printf("gateway read error (gw=%s): %v", gw.ID, err)
			break
		}

		frame, err := parseRelayFrame(raw)
		if err != nil {
			// not a relay frame, treat as broadcast to all clients
			h.broadcastToClients(gw.ID, raw)
			continue
		}

		// targeted forward: frame.From should be "client:<deviceToken>" indicating target
		// But from gateway side, frame.From indicates the target client
		h.forwardToClient(frame.From, gw.ID, frame.Data)
	}

	h.unregisterGateway(gw.ID)
}

// HandleClientConnect handles WebSocket connections from clients using a connect code.
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

	h.runClientLoop(cs)
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

	sendControlMessage(ws, "reconnected", map[string]string{
		"gatewayId": gwID,
	})

	h.runClientLoop(cs)
}

func (h *Hub) runClientLoop(cs *ClientSession) {
	done := make(chan struct{})
	defer close(done)
	go startPinger(cs.Conn, done)

	cs.Conn.setReadDeadline(timeNow().Add(pongWait))
	cs.Conn.setPongHandler(func(string) error {
		cs.Conn.setReadDeadline(timeNow().Add(pongWait))
		return nil
	})

	for {
		_, raw, err := cs.Conn.readMessage()
		if err != nil {
			log.Printf("client read error (device=%s): %v", cs.DeviceToken, err)
			break
		}
		// forward to gateway
		h.forwardToGateway(cs.GatewayID, cs.DeviceToken, raw)
	}

	h.removeClient(cs.DeviceToken, cs.GatewayID)
}

// forwardToGateway sends a client message to the gateway via relay frame.
func (h *Hub) forwardToGateway(gwID, deviceToken string, data []byte) {
	h.mu.RLock()
	gw, ok := h.gateways[gwID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	if err := sendRelayFrame(gw.Conn, "client:"+deviceToken, data); err != nil {
		log.Printf("forward to gateway failed (gw=%s): %v", gwID, err)
	}
}

// forwardToClient sends a gateway message to a specific client.
func (h *Hub) forwardToClient(target, gwID string, data []byte) {
	// target format: "client:<deviceToken>"
	if len(target) <= 7 {
		return
	}
	deviceToken := target[7:]
	key := clientKey(deviceToken, gwID)

	h.mu.RLock()
	cs, ok := h.clients[key]
	h.mu.RUnlock()
	if !ok {
		return
	}
	if err := cs.Conn.writeMessage(websocket.TextMessage, data); err != nil {
		log.Printf("forward to client failed (device=%s): %v", deviceToken, err)
	}
}

// broadcastToClients sends raw data to all clients of a gateway.
func (h *Hub) broadcastToClients(gwID string, data []byte) {
	h.mu.RLock()
	gw, ok := h.gateways[gwID]
	if !ok {
		h.mu.RUnlock()
		return
	}
	// copy client list to avoid holding lock during writes
	clients := make([]*ClientSession, 0, len(gw.clients))
	for _, cs := range gw.clients {
		clients = append(clients, cs)
	}
	h.mu.RUnlock()

	for _, cs := range clients {
		if err := cs.Conn.writeMessage(websocket.TextMessage, data); err != nil {
			log.Printf("broadcast to client failed (device=%s): %v", cs.DeviceToken, err)
		}
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
