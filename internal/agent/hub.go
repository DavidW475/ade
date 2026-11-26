package agent

import (
	"context"
	"sync"
	"time"

	"github.com/DavidW475/ade/internal/store"
)

// Grenzen für den Event-Strom.
const (
	// DefaultFlushInterval ist die längste Zeit, die ein Event ungespeichert im Speicher liegt.
	DefaultFlushInterval = 100 * time.Millisecond
	// flushBatch: ab so vielen ausstehenden Events wird sofort geschrieben.
	flushBatch = 256
	// subBuffer ist der Puffer pro Abonnent; wer nicht hinterherkommt, wird getrennt und
	// verbindet sich mit ?since=<seq> neu.
	subBuffer = 1024
)

// hub verteilt die Events einer Session live an Abonnenten und schreibt sie gebündelt in die
// Datenbank. seq wird hier vergeben, deshalb gibt es pro Session genau einen hub.
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

// publish vergibt die nächste seq, verteilt das Event und plant das Schreiben.
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
		// Events nicht verwerfen: beim nächsten Mal erneut versuchen.
		if h.timer == nil {
			h.timer = time.AfterFunc(time.Second, h.flush)
		}
		return
	}
	h.pending = h.pending[:0]
}

// subscribe schreibt ausstehende Events, meldet einen neuen Abonnenten an und liefert die
// seq, bis zu der alle Events in der Datenbank stehen. Alle Events im Kanal haben eine
// größere seq – zusammen mit einem Replay aus der Datenbank geht nichts verloren.
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

// Subscription ist ein Live-Abonnement der Events einer Session.
type Subscription struct {
	ch     chan store.Event
	h      *hub
	lagged bool
	// release meldet dem Manager, dass das Abonnement endet.
	release func()
	once    sync.Once
}

// C liefert die Live-Events. Der Kanal wird geschlossen, wenn das Abonnement endet
// (Close oder der Abonnent kam nicht hinterher, siehe Lagged).
func (s *Subscription) C() <-chan store.Event { return s.ch }

// Lagged meldet, ob das Abonnement beendet wurde, weil der Abonnent zu langsam war.
func (s *Subscription) Lagged() bool {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.lagged
}

// Close beendet das Abonnement.
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
