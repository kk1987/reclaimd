package reclaimd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// maxSSEClients bounds the memory a forgotten browser tab can cost. Each client
// is one goroutine plus a small buffer, so eight is a few tens of kilobytes --
// which matters on a router sharing its RAM with the thing it routes for.
const maxSSEClients = 8

// replayRing is how many recent frames are kept for Last-Event-ID resume. This
// is the one thing SSE gives us for free that polling would have to reinvent.
const replayRing = 256

// Frame is one server-sent event.
type Frame struct {
	Event string
	ID    uint64
	Data  []byte
}

// Hub fans frames out to connected browsers.
//
// The coalescing that keeps a 100-events-per-second scanner from flooding a
// 2 Hz UI happens in the publisher, not here: progress frames are idempotent
// snapshots, so the newest one is always the only one worth sending.
type Hub struct {
	mu      sync.Mutex
	clients map[chan Frame]struct{}
	ring    []Frame
	nextID  uint64
}

func NewHub() *Hub {
	return &Hub{clients: map[chan Frame]struct{}{}, ring: make([]Frame, 0, replayRing)}
}

func (h *Hub) Subscribe(lastID uint64) (<-chan Frame, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= maxSSEClients {
		return nil, nil, false
	}
	ch := make(chan Frame, 8)
	h.clients[ch] = struct{}{}

	// Replay anything the client missed while reconnecting.
	for _, f := range h.ring {
		if f.ID > lastID {
			select {
			case ch <- f:
			default:
			}
		}
	}

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
	}
	return ch, cancel, true
}

// Publish delivers a frame, dropping progress frames under backpressure.
//
// A progress frame is a full snapshot, so discarding a stale one loses nothing.
// Events and lifecycle changes are not droppable -- a missed dropout would
// leave the UI quietly wrong -- so a client that cannot keep up with those gets
// told to resynchronise instead.
func (h *Hub) Publish(event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	f := Frame{Event: event, ID: h.nextID, Data: b}

	if event != "progress" {
		h.ring = append(h.ring, f)
		if len(h.ring) > replayRing {
			h.ring = h.ring[len(h.ring)-replayRing:]
		}
	}

	for ch := range h.clients {
		select {
		case ch <- f:
		default:
			if event == "progress" {
				continue // stale snapshot, nothing lost
			}
			select {
			case ch <- Frame{Event: "resync", ID: f.ID, Data: []byte(`{}`)}:
			default:
			}
		}
	}
}

// ServeSSE streams frames to one browser.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request, hello any) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var lastID uint64
	fmt.Sscanf(r.Header.Get("Last-Event-ID"), "%d", &lastID)

	ch, cancel, ok := h.Subscribe(lastID)
	if !ok {
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	fmt.Fprintf(w, "retry: 5000\n")
	if b, err := json.Marshal(hello); err == nil {
		fmt.Fprintf(w, "event: hello\ndata: %s\n\n", b)
	}
	flusher.Flush()

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	// An absolute cap catches the cases a keepalive cannot: a laptop lid closed
	// mid-stream, a proxy that never signals close. EventSource reconnects on
	// its own, so this costs the client nothing.
	deadline := time.After(2 * time.Hour)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case f, ok := <-ch:
			if !ok {
				return
			}
			if f.Event != "progress" {
				fmt.Fprintf(w, "id: %d\n", f.ID)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.Event, f.Data)
			flusher.Flush()
		}
	}
}
