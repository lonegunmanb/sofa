package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/dapr/go-sdk/service/common"
	"github.com/lonegunmanb/sofa/internal/actor/run"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/lonegunmanb/sofa/internal/worker/daemon"
)

const defaultWSClientBuffer = 64

// WSEvent is the unified live event envelope streamed over /api/v1/ws.
type WSEvent struct {
	Topic     string          `json:"topic"`
	Type      string          `json:"type,omitempty"`
	RunID     string          `json:"run_id,omitempty"`
	NodeAddr  string          `json:"node_addr,omitempty"`
	Epoch     uint64          `json:"epoch,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type wsClient struct {
	runID  string
	ch     chan []byte
	cancel context.CancelFunc
}

// WSHub fans out Dapr PubSub events to global and run_id-filtered WebSocket subscribers.
type WSHub struct {
	mu         sync.RWMutex
	clients    map[*wsClient]struct{}
	closed     bool
	bufferSize int
}

// NewWSHub constructs a thread-safe WebSocket fan-out hub with bounded per-client buffers.
func NewWSHub(bufferSize int) *WSHub {
	if bufferSize <= 0 {
		bufferSize = defaultWSClientBuffer
	}
	return &WSHub{
		clients:    make(map[*wsClient]struct{}),
		bufferSize: bufferSize,
	}
}

func (h *WSHub) subscribe(runID string, cancel context.CancelFunc) (*wsClient, <-chan []byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, false
	}
	c := &wsClient{
		runID:  strings.TrimSpace(runID),
		ch:     make(chan []byte, h.bufferSize),
		cancel: cancel,
	}
	h.clients[c] = struct{}{}
	return c, c.ch, true
}

func (h *WSHub) unsubscribe(c *wsClient) {
	if c == nil {
		return
	}
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.ch)
	}
	h.mu.Unlock()
}

// Publish broadcasts ev to all matching subscribers without blocking on slow clients.
func (h *WSHub) Publish(ev WSEvent) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}

	var slow []*wsClient
	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return
	}
	for c := range h.clients {
		if c.runID != "" && c.runID != ev.RunID {
			continue
		}
		select {
		case c.ch <- raw:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range slow {
		if c.cancel != nil {
			c.cancel()
		}
	}
}

// Close closes all active subscriber streams.
func (h *WSHub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
		delete(h.clients, c)
		close(c.ch)
	}
	h.mu.Unlock()

	for _, c := range clients {
		if c.cancel != nil {
			c.cancel()
		}
	}
}

// HandlePubSubEvent normalizes raw Dapr PubSub payloads from sofa-events or sofa.run.events and fans them out.
func (h *WSHub) HandlePubSubEvent(_ context.Context, topic string, rawData []byte) error {
	trimmed := strings.TrimSpace(string(rawData))
	if trimmed == "" {
		return nil
	}
	var meta struct {
		RunID     string     `json:"run_id"`
		NodeAddr  string     `json:"node_addr"`
		Epoch     uint64     `json:"epoch"`
		Kind      string     `json:"kind"`
		Status    string     `json:"status"`
		Timestamp *time.Time `json:"timestamp"`
		UpdatedAt *time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal([]byte(trimmed), &meta); err != nil {
		return fmt.Errorf("server: decode pubsub event on %q: %w", topic, err)
	}
	evType := meta.Kind
	if evType == "" && meta.Status != "" {
		evType = "run_status:" + meta.Status
	}
	ts := time.Now().UTC()
	if meta.Timestamp != nil && !meta.Timestamp.IsZero() {
		ts = meta.Timestamp.UTC()
	} else if meta.UpdatedAt != nil && !meta.UpdatedAt.IsZero() {
		ts = meta.UpdatedAt.UTC()
	}

	h.Publish(WSEvent{
		Topic:     topic,
		Type:      evType,
		RunID:     meta.RunID,
		NodeAddr:  meta.NodeAddr,
		Epoch:     meta.Epoch,
		Timestamp: ts,
		Payload:   json.RawMessage(trimmed),
	})
	return nil
}

// registerPubSubHandlers subscribes the internal Dapr service to sofa-events and sofa.run.events.
func (s *Server) registerPubSubHandlers(svc common.Service) error {
	subs := []struct {
		topic string
		route string
	}{
		{topic: daemon.EventsTopic, route: "/pubsub/sofa-events"},
		{topic: run.DefaultRunEventsTopic, route: "/pubsub/sofa-run-events"},
	}
	for _, sub := range subs {
		topic := sub.topic
		if err := svc.AddTopicEventHandler(&common.Subscription{
			PubsubName: daprhost.PubSubComponentName,
			Topic:      topic,
			Route:      sub.route,
		}, func(ctx context.Context, e *common.TopicEvent) (bool, error) {
			if e == nil {
				return false, nil
			}
			_ = s.hub.HandlePubSubEvent(ctx, topic, e.RawData)
			return false, nil
		}); err != nil {
			return fmt.Errorf("server: subscribe topic %q: %w", topic, err)
		}
	}
	return nil
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	var subprotocols []string
	if s.cfg.APIToken != "" {
		tok, offerSofaV1, hasToken := extractWebSocketAuth(r)
		if !hasToken || !constantTimeEqual(tok, s.cfg.APIToken) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if offerSofaV1 {
			subprotocols = []string{WebSocketSubprotocol}
		}
	} else {
		_, offerSofaV1, _ := extractWebSocketAuth(r)
		if offerSofaV1 {
			subprotocols = []string{WebSocketSubprotocol}
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: subprotocols,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	connCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
	client, ch, ok := s.hub.subscribe(runID, cancel)
	if !ok {
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	defer s.hub.unsubscribe(client)

	// Drain client control/incoming frames so pings, pongs, and close frames are processed promptly.
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(connCtx); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-connCtx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		case msg, open := <-ch:
			if !open {
				_ = conn.Close(websocket.StatusGoingAway, "hub closed")
				return
			}
			writeCtx, writeCancel := context.WithTimeout(connCtx, 5*time.Second)
			wErr := conn.Write(writeCtx, websocket.MessageText, msg)
			writeCancel()
			if wErr != nil {
				return
			}
		}
	}
}
