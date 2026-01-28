package relay

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// wsConn wraps a gorilla websocket with a write mutex and close-once.
type wsConn struct {
	conn     *websocket.Conn
	writeMu  sync.Mutex
	closed   bool
	closeMu  sync.Mutex
}

func newWsConn(c *websocket.Conn) *wsConn {
	return &wsConn{conn: c}
}

func (w *wsConn) writeMessage(msgType int, data []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return w.conn.WriteMessage(msgType, data)
}

func (w *wsConn) readMessage() (int, []byte, error) {
	return w.conn.ReadMessage()
}

func (w *wsConn) close() {
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	w.conn.Close()
}

func (w *wsConn) setPongHandler(f func(string) error) {
	w.conn.SetPongHandler(f)
}

func (w *wsConn) setReadDeadline(t time.Time) {
	w.conn.SetReadDeadline(t)
}

// controlMessage is a JSON envelope for relay control messages.
type controlMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload,omitempty"`
}

func sendControlMessage(conn *wsConn, msgType string, payload interface{}) error {
	data, err := json.Marshal(controlMessage{Type: msgType, Payload: payload})
	if err != nil {
		return err
	}
	return conn.writeMessage(websocket.TextMessage, data)
}

// relayFrame wraps a client/gateway data message for relay forwarding.
// The relay prepends a thin envelope so the other side knows the source.
type relayFrame struct {
	From string `json:"from"` // "client:<deviceToken>" or "gateway"
	Data []byte `json:"data"`
}

func sendRelayFrame(conn *wsConn, from string, data []byte) error {
	frame, err := json.Marshal(relayFrame{From: from, Data: data})
	if err != nil {
		return err
	}
	return conn.writeMessage(websocket.TextMessage, frame)
}

// parseRelayFrame decodes a relay frame envelope.
func parseRelayFrame(raw []byte) (*relayFrame, error) {
	var f relayFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

const (
	pingInterval = 30 * time.Second
	pongWait     = 60 * time.Second
)

func startPinger(conn *wsConn, done <-chan struct{}) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			conn.writeMu.Lock()
			conn.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := conn.conn.WriteMessage(websocket.PingMessage, nil)
			conn.writeMu.Unlock()
			if err != nil {
				log.Printf("ping failed: %v", err)
				return
			}
		case <-done:
			return
		}
	}
}
