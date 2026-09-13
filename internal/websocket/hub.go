package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Rahulrajln1111/chitchat/internal/auth"
	"github.com/Rahulrajln1111/chitchat/internal/messages"
	"github.com/gorilla/websocket"
	"github.com/lib/pq"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// client is one authenticated WebSocket connection.
type client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	username string
}

// Hub manages local room sessions and cross-instance fan-out via PostgreSQL NOTIFY.
type Hub struct {
	db        *sql.DB
	svc       *messages.Service
	jwtSecret string

	mu     sync.RWMutex
	rooms  map[string]map[*client]bool // roomID -> set of local clients
	global map[*client]bool

	register   chan *client
	unregister chan *client
}

// NewHub creates the hub. Start Run() in a goroutine, then StartListener().
func NewHub(database *sql.DB, svc *messages.Service, jwtSecret string) *Hub {
	return &Hub{
		db:        database,
		svc:       svc,
		jwtSecret: jwtSecret,
		rooms:     make(map[string]map[*client]bool),
		global:    make(map[*client]bool),
		register:  make(chan *client, 256),
		unregister: make(chan *client, 256),
	}
}

// ServeWS upgrades and authenticates the connection (Java parity with JWTHandshakeInterceptor).
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	tokenStr := r.URL.Query().Get("token")
	if tokenStr == "" {
		http.Error(w, "token required", http.StatusUnauthorized)
		return
	}
	claims, err := auth.ValidateToken(h.jwtSecret, tokenStr)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	c := &client{
		hub:      h,
		conn:     conn,
		send:     make(chan []byte, 512),
		username: claims.Username,
	}
	h.register <- c

	go c.writePump()
	go c.readPump()
}

// Run processes register/unregister events.
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
			h.global[c] = true
			h.mu.Unlock()

		case c := <-h.unregister:
			h.mu.Lock()
			delete(h.global, c)
			for roomID, set := range h.rooms {
				delete(set, c)
				if len(set) == 0 {
					delete(h.rooms, roomID)
				}
			}
			h.mu.Unlock()
			close(c.send)
		}
	}
}

// readPump reads inbound frames and dispatches by type (Java ChatWebSocketHandler parity).
func (c *client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(1 << 20)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		var base struct {
			Type   string `json:"type"`
			RoomID string `json:"roomId"`
		}
		if err := json.Unmarshal(raw, &base); err != nil {
			continue
		}

		switch base.Type {
		case "JOIN_ROOM":
			// Only members may join a room session (Java parity)
			if !c.hub.svc.IsUserMember(context.Background(), c.username, base.RoomID) {
				continue
			}
			c.hub.joinRoomLocal(base.RoomID, c)

		case "SEND_MESSAGE":
			var req struct {
				RoomID  string `json:"roomId"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &req); err != nil || req.RoomID == "" {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			// Membership check then encrypted persist (Java saveMessage parity)
			if !c.hub.svc.IsUserMember(ctx, c.username, req.RoomID) {
				cancel()
				continue
			}
			resp, err := c.hub.svc.SaveMessage(ctx, req.RoomID, c.username, req.Content)
			cancel()
			if err != nil {
				log.Printf("[Hub] save message failed: %v", err)
				continue
			}
			// Fan out via NOTIFY; every instance (incl. this one) delivers locally.
			notifyMSG(c.hub.db, req.RoomID, resp.MessageID)

		case "MESSAGE_DELIVERED":
			var req struct {
				RoomID    string `json:"roomId"`
				MessageID string `json:"messageId"`
			}
			if json.Unmarshal(raw, &req) != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			c.hub.svc.UpdateDelivered(ctx, req.MessageID, req.RoomID, c.username)
			cancel()
			notifySTA(c.hub.db, req.RoomID, req.MessageID, c.username)

		case "MESSAGE_READ":
			var req struct {
				RoomID    string `json:"roomId"`
				MessageID string `json:"messageId"`
			}
			if json.Unmarshal(raw, &req) != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			c.hub.svc.UpdateRead(ctx, req.MessageID, req.RoomID, c.username)
			cancel()
			notifySTA(c.hub.db, req.RoomID, req.MessageID, c.username)
		}
	}
}

// writePump writes outbound frames with keepalive pings.
func (c *client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (h *Hub) joinRoomLocal(roomID string, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[roomID] == nil {
		h.rooms[roomID] = make(map[*client]bool)
	}
	h.rooms[roomID][c] = true
}

// broadcastLocal delivers pre-serialized JSON to local room sockets (prunes dead).
func (h *Hub) broadcastLocal(roomID string, payload []byte) {
	h.mu.RLock()
	set := h.rooms[roomID]
	clients := make([]*client, 0, len(set))
	for c := range set {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		select {
		case c.send <- payload:
		default:
			// slow consumer: drop connection like Java prunes dead sockets
			h.unregister <- c
		}
	}
}

// StartListener LISTENs on the PostgreSQL channel and re-dispatches events to
// local sockets. This is what makes chat work ACROSS backends (Java WsEventListener parity).
func (h *Hub) StartListener(databaseURL string) {
	go func() {
		minReconnect := 10 * time.Second
		maxReconnect := time.Minute
		listener := pq.NewListener(databaseURL, minReconnect, maxReconnect,
			func(ev pq.ListenerEventType, err error) {
				if err != nil {
					log.Printf("[WsListener] event: %v", err)
				}
			})
		if err := listener.Listen("chitchat_ws"); err != nil {
			log.Printf("[WsListener] cannot listen: %v", err)
			return
		}
		defer listener.Close()
		log.Println("[WsListener] LISTENing on channel 'chitchat_ws'")

		for {
			select {
			case n := <-listener.Notify:
				h.handleNotify(n.Channel, n.Extra)
			case <-time.After(90 * time.Second):
				go listener.Ping() // keep the connection healthy
			}
		}
	}()
}

// notifyEnvelope mirrors Java WsEventPublisher.Envelope.
type notifyEnvelope struct {
	T string `json:"t"` // MSG | STA | SYS
	R string `json:"r"` // room id
	M string `json:"m"` // message id
	U string `json:"u"` // username
	P string `json:"p"` // inline payload (SYS only)
}

func (h *Hub) handleNotify(channel, payload string) {
	if channel != "chitchat_ws" {
		return
	}
	var env notifyEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch env.T {
	case "MSG":
		if msg := h.svc.GetLiveMessage(ctx, env.M); msg != nil {
			if b, err := json.Marshal(msg); err == nil {
				h.broadcastLocal(env.R, b)
			}
		}
	case "STA":
		if st := h.svc.GetStatusUpdate(ctx, env.M, env.R, env.U); st != nil {
			if b, err := json.Marshal(st); err == nil {
				h.broadcastLocal(env.R, b)
			}
		}
	case "SYS":
		// ephemeral join/leave events travel inline
		h.broadcastLocal(env.R, []byte(env.P))
	}
}

// publishNotify is the shared NOTIFY sender (Java WsEventPublisher parity).
func publishNotify(database *sql.DB, env notifyEnvelope) {
	payload, err := json.Marshal(env)
	if err != nil || len(payload) > 7500 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, "SELECT pg_notify($1, $2)", "chitchat_ws", string(payload)); err != nil {
		log.Printf("[WsPublisher] failed to publish event: %v", err)
	}
}

func notifyMSG(database *sql.DB, roomID, messageID string) {
	publishNotify(database, notifyEnvelope{T: "MSG", R: roomID, M: messageID})
}

func notifySTA(database *sql.DB, roomID, messageID, username string) {
	publishNotify(database, notifyEnvelope{T: "STA", R: roomID, M: messageID, U: username})
}

// NotifySystem lets room handlers broadcast join/leave system events.
func NotifySystem(database *sql.DB, roomID string, event interface{}) {
	if b, err := json.Marshal(event); err == nil {
		publishNotify(database, notifyEnvelope{T: "SYS", R: roomID, P: string(b)})
	}
}

// pqNotification handling is done via pq.Listener (lib/pq) in StartListener.
