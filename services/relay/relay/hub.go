package relay

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"sync"
	"time"
)

// Hub manages gateway registrations and client-gateway pairings.
type Hub struct {
	secret string

	mu       sync.RWMutex
	gateways map[string]*Gateway        // gatewayId -> Gateway
	codes    map[string]*PendingCode     // connectCode -> PendingCode
	clients  map[string]*ClientSession   // clientKey -> ClientSession

	done chan struct{}
	wg   sync.WaitGroup
}

type Gateway struct {
	ID        string
	Conn      *wsConn
	CreatedAt time.Time
	// all client connections relayed through this gateway
	clients map[string]*ClientSession
}

type PendingCode struct {
	Code      string
	GatewayID string
	ExpiresAt time.Time
}

type ClientSession struct {
	DeviceToken string
	GatewayID   string
	Conn        *wsConn
}

func NewHub(secret string) *Hub {
	h := &Hub{
		secret:   secret,
		gateways: make(map[string]*Gateway),
		codes:    make(map[string]*PendingCode),
		clients:  make(map[string]*ClientSession),
		done:     make(chan struct{}),
	}
	h.wg.Add(1)
	go h.cleanupLoop()
	return h
}

func (h *Hub) Close() {
	close(h.done)
	h.wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, gw := range h.gateways {
		gw.Conn.close()
	}
	for _, cs := range h.clients {
		cs.Conn.close()
	}
}

// generateID returns a random hex string.
func generateID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// generateConnectCode returns a short uppercase alphanumeric code like "ABCD-1234".
func generateConnectCode() string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no ambiguous chars
	b := make([]byte, 8)
	rand.Read(b)
	code := make([]byte, 9) // 4 + dash + 4
	for i := 0; i < 4; i++ {
		code[i] = chars[int(b[i])%len(chars)]
	}
	code[4] = '-'
	for i := 0; i < 4; i++ {
		code[5+i] = chars[int(b[4+i])%len(chars)]
	}
	return string(code)
}

func (h *Hub) registerGateway(conn *wsConn, token string) (*Gateway, string, error) {
	if token != h.secret {
		return nil, "", errUnauthorized
	}
	gwID := generateID(16)
	code := generateConnectCode()

	h.mu.Lock()
	defer h.mu.Unlock()

	gw := &Gateway{
		ID:        gwID,
		Conn:      conn,
		CreatedAt: time.Now(),
		clients:   make(map[string]*ClientSession),
	}
	h.gateways[gwID] = gw
	h.codes[code] = &PendingCode{
		Code:      code,
		GatewayID: gwID,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	log.Printf("gateway registered: id=%s", gwID)
	return gw, code, nil
}

func (h *Hub) unregisterGateway(gwID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	gw, ok := h.gateways[gwID]
	if !ok {
		return
	}
	// notify all clients that gateway disconnected
	for _, cs := range gw.clients {
		sendControlMessage(cs.Conn, "gateway_disconnected", nil)
		cs.Conn.close()
		delete(h.clients, clientKey(cs.DeviceToken, gwID))
	}
	delete(h.gateways, gwID)

	// clean up any pending codes for this gateway
	for code, pc := range h.codes {
		if pc.GatewayID == gwID {
			delete(h.codes, code)
		}
	}
	log.Printf("gateway unregistered: id=%s", gwID)
}

func (h *Hub) pairClient(conn *wsConn, code string) (*ClientSession, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	pc, ok := h.codes[code]
	if !ok || time.Now().After(pc.ExpiresAt) {
		if ok {
			delete(h.codes, code)
		}
		return nil, "", errInvalidCode
	}

	gw, ok := h.gateways[pc.GatewayID]
	if !ok {
		delete(h.codes, code)
		return nil, "", errGatewayGone
	}

	deviceToken := generateID(16)
	cs := &ClientSession{
		DeviceToken: deviceToken,
		GatewayID:   pc.GatewayID,
		Conn:        conn,
	}
	key := clientKey(deviceToken, pc.GatewayID)
	h.clients[key] = cs
	gw.clients[key] = cs

	// code is single-use
	delete(h.codes, code)

	log.Printf("client paired: gw=%s device=%s", pc.GatewayID, deviceToken)
	return cs, pc.GatewayID, nil
}

func (h *Hub) reconnectClient(conn *wsConn, gwID, deviceToken string) (*ClientSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	gw, ok := h.gateways[gwID]
	if !ok {
		return nil, errGatewayGone
	}

	key := clientKey(deviceToken, gwID)
	// close old connection if exists
	if old, ok := h.clients[key]; ok {
		old.Conn.close()
	}

	cs := &ClientSession{
		DeviceToken: deviceToken,
		GatewayID:   gwID,
		Conn:        conn,
	}
	h.clients[key] = cs
	gw.clients[key] = cs

	log.Printf("client reconnected: gw=%s device=%s", gwID, deviceToken)
	return cs, nil
}

func (h *Hub) removeClient(deviceToken, gwID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	key := clientKey(deviceToken, gwID)
	delete(h.clients, key)
	if gw, ok := h.gateways[gwID]; ok {
		delete(gw.clients, key)
	}
	log.Printf("client removed: gw=%s device=%s", gwID, deviceToken)
}

func (h *Hub) cleanupLoop() {
	defer h.wg.Done()
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			h.mu.Lock()
			now := time.Now()
			for code, pc := range h.codes {
				if now.After(pc.ExpiresAt) {
					delete(h.codes, code)
				}
			}
			h.mu.Unlock()
		}
	}
}

func clientKey(deviceToken, gwID string) string {
	return deviceToken + ":" + gwID
}
