// Package web — events.go is a tiny in-process event broker that lets
// the dashboard skip polling. Server code calls Publish("questionsChanged")
// after mutating state; every connected browser receives an SSE
// event within milliseconds, which htmx turns into a targeted
// hx-trigger refresh.
//
// Design notes:
//
//   - One goroutine per connected client, holding a buffered channel.
//     When the channel fills (slow consumer), the broker drops events
//     for that subscriber rather than blocking publishers. The dropped
//     consumer just gets a stale view for one heartbeat; the next
//     change re-syncs them. Better than blocking the workflow engine.
//
//   - Heartbeat every 25s keeps proxies from closing idle connections
//     (most stacks drop idle SSE after 30–60s).
//
//   - No replay log. Subscribers that connect mid-flight just see the
//     next change. The dashboard always renders from `Memory` on
//     first load, so missing intermediate events is fine.
//
//   - Event NAMES match the existing HX-Trigger event names used
//     across handlers (`questionsChanged`, `workflowsChanged`, etc.)
//     so we can drop polling without touching every fragment.
package web

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// eventBus is the broker. Zero value is unusable — go through newEventBus.
type eventBus struct {
	mu     sync.Mutex
	subs   map[uint64]chan string
	nextID atomic.Uint64
}

func newEventBus() *eventBus {
	return &eventBus{subs: make(map[uint64]chan string)}
}

// Publish sends an event name to every subscriber. Non-blocking: if a
// subscriber's channel is full we drop the event for them rather than
// stall publishers. Safe to call from any goroutine.
func (b *eventBus) Publish(name string) {
	if b == nil || name == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- name:
		default:
			// subscriber is slow — skip them this tick.
		}
	}
}

// subscribe returns a channel that receives every Publish + an
// unsubscribe func. Channel buffer is 16; bursts beyond that are
// dropped for that subscriber.
func (b *eventBus) subscribe() (uint64, chan string, func()) {
	id := b.nextID.Add(1)
	ch := make(chan string, 16)
	b.mu.Lock()
	b.subs[id] = ch
	b.mu.Unlock()
	return id, ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// handleEvents is the SSE endpoint. Each connected browser keeps one
// long-lived GET open; the server writes `event: <name>\ndata: 1\n\n`
// for every published change. A small JS shim in index.html dispatches
// those events onto document.body so htmx's existing
// `hx-trigger="<name> from:body"` machinery picks them up unchanged.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		http.Error(w, "events bus not initialised", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: don't buffer SSE
	// Initial comment so the connection commits before the first
	// real event.
	fmt.Fprintf(w, ": connected at %d\n\n", time.Now().Unix())
	flusher.Flush()

	_, ch, unsub := s.events.subscribe()
	defer unsub()

	// Heartbeat ticker keeps proxies happy on long idle stretches.
	hb := time.NewTicker(25 * time.Second)
	defer hb.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case name, ok := <-ch:
			if !ok {
				return
			}
			// SSE wire format. data must be non-empty for some
			// browsers to dispatch the event; "1" is fine.
			fmt.Fprintf(w, "event: %s\ndata: 1\n\n", name)
			flusher.Flush()
		case <-hb.C:
			// Comment line: keeps the connection alive, doesn't
			// trigger any client-side event.
			fmt.Fprintf(w, ": heartbeat %d\n\n", time.Now().Unix())
			flusher.Flush()
		}
	}
}

// publishStatus is a convenience for the daemon's status changes.
// Kept separate from inline Publish calls so the call sites read
// declaratively.
func (s *Server) publishStatus() {
	if s.events != nil {
		s.events.Publish("statusChanged")
	}
}

// Compile-time ensure the broker satisfies whatever interface the
// daemon side wants when we wire it through later. Currently no
// shared interface — callers just hold *eventBus directly.
var _ = context.Background