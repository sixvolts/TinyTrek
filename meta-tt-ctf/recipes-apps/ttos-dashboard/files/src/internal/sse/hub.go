// Package sse is a minimal Server-Sent Events fan-out hub.
//
// A read-only monitor only pushes server->client, so SSE over net/http is a
// perfect fit and needs no websocket dependency. Slow clients are dropped rather
// than allowed to apply backpressure to the CAN readers -- a monitor tolerates
// dropping frames far better than it tolerates stalling the bus reader.
package sse

// Client is a per-connection channel of pre-encoded SSE payloads.
type Client chan []byte

// Hub tracks connected clients and broadcasts messages to them.
type Hub struct {
	register   chan Client
	unregister chan Client
	broadcast  chan []byte
	clients    map[Client]struct{}
}

func NewHub() *Hub {
	return &Hub{
		register:   make(chan Client),
		unregister: make(chan Client),
		broadcast:  make(chan []byte, 1024),
		clients:    make(map[Client]struct{}),
	}
}

// Run owns the client set; call once in its own goroutine.
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = struct{}{}
		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c)
			}
		case msg := <-h.broadcast:
			for c := range h.clients {
				select {
				case c <- msg:
				default: // client is behind; drop this frame for it
				}
			}
		}
	}
}

// Publish queues a message for broadcast. Non-blocking: if the broadcast buffer
// is full the message is dropped rather than stalling the caller (a CAN reader).
func (h *Hub) Publish(msg []byte) {
	select {
	case h.broadcast <- msg:
	default:
	}
}

// Add registers a new client and returns its channel.
func (h *Hub) Add() Client {
	c := make(Client, 256)
	h.register <- c
	return c
}

// Remove unregisters a client.
func (h *Hub) Remove(c Client) { h.unregister <- c }

// Count is not exported as live state here; the dashboard tracks counts itself.
