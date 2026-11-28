package agent

import (
	"context"
	"sync"
	"time"

	"github.com/DavidW475/ade/internal/store"
)

// Limits for the event stream.
const (
	// DefaultFlushInterval is the longest time an event stays in memory unstored.
	DefaultFlushInterval = 100 * time.Millisecond
	// flushBatch: from this many pending events on, write right away.
	flushBatch = 256
	// subBuffer is the buffer per subscriber; whoever cannot keep up is disconnected and
	// reconnects with ?since=<seq>.
	subBuffer = 1024
)

// hub distributes a session's events live to subscribers and writes them to the database in
// batches. seq is assigned here, which is why there is exactly one hub per session.
type hub struct {
	id       string
	st       *store.Store
	interval time.Duration
	onError  func(error)

	mu      sync.Mutex
	seq     int64
	pending []store.Event
	timer   *time.Timer
	subs    map[*Subscription]struct{}
}

func newHub(st *store.Store, id string, seq int64, interval time.Duration, onError func(error)) *hub {
	return &hub{id: id, st: st, seq: seq, interval: interval, onError: onError, subs: map[*Subscription]struct{}{}}
}

// publish assigns the next seq, distributes the event and schedules the write.
func (h *hub) publish(typ string, payload []byte) store.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev := store.Event{Seq: h.seq, SessionID: h.id, TS: time.Now().UTC(), Type: typ, Payload: payload}
	h.pending = append(h.pending, ev)
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default:
			s.lagged = true
			h.dropLocked(s)
		}
	}
	switch {
	case len(h.pending) >= flushBatch:
		h.flushLocked()
	case h.timer == nil:
		h.timer = time.AfterFunc(h.interval, h.flush)
	}
	return ev
}

func (h *hub) flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushLocked()
}

func (h *hub) flushLocked() {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	if len(h.pending) == 0 {
		return
	}
	if err := h.st.AppendEvents(context.Background(), h.pending); err != nil {
		h.onError(err)
		// Do not discard the events: try again next time.
		if h.timer == nil {
			h.timer = time.AfterFunc(time.Second, h.flush)
		}
		return
	}
	h.pending = h.pending[:0]
}

// subscribe writes pending events, registers a new subscriber and returns the seq up to
// which all events are in the database. All events in the channel have a greater seq –
// together with a replay from the database nothing is lost.
func (h *hub) subscribe() (*Subscription, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushLocked()
	s := &Subscription{ch: make(chan store.Event, subBuffer), h: h}
	h.subs[s] = struct{}{}
	return s, h.seq
}

func (h *hub) dropLocked(s *Subscription) {
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
}

func (h *hub) idle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) == 0 && len(h.pending) == 0
}

// Subscription is a live subscription to the events of a session.
type Subscription struct {
	ch     chan store.Event
	h      *hub
	lagged bool
	// release tells the manager that the subscription ends.
	release func()
	once    sync.Once
}

// C returns the live events. The channel is closed when the subscription ends (Close or
// the subscriber could not keep up, see Lagged).
func (s *Subscription) C() <-chan store.Event { return s.ch }

// Lagged reports whether the subscription ended because the subscriber was too slow.
func (s *Subscription) Lagged() bool {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.lagged
}

// Close ends the subscription.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.h.mu.Lock()
		s.h.dropLocked(s)
		s.h.mu.Unlock()
		if s.release != nil {
			s.release()
		}
	})
}
