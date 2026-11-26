package agent

import (
	"io"
	"os"
	"sync"
)

// Grenzen für Terminal-Ausgaben.
const (
	// TerminalReplay ist die Menge an Ausgabe, die ein neu verbundener Client zuerst bekommt.
	TerminalReplay = 256 << 10
	// MaxTerminalLog begrenzt die Log-Datei einer Terminal-Session; danach wird nur noch
	// der Puffer im Speicher aktualisiert.
	MaxTerminalLog   = 32 << 20
	termClientBuffer = 256
)

// terminal puffert die Ausgabe eines PTY: die letzten TerminalReplay Bytes im Speicher für
// neu verbundene Clients und die gesamte Ausgabe in einer Datei, damit sie auch nach
// Prozessende oder Daemon-Neustart sichtbar bleibt.
type terminal struct {
	mu      sync.Mutex
	ring    []byte
	log     *os.File
	written int64
	clients map[*termClient]struct{}
	closed  bool
}

type termClient struct {
	ch chan []byte
}

func newTerminal(logPath string) *terminal {
	t := &terminal{clients: map[*termClient]struct{}{}}
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		t.log = f
		if st, err := f.Stat(); err == nil {
			t.written = st.Size()
		}
	}
	return t
}

func (t *terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ring = append(t.ring, p...)
	if over := len(t.ring) - TerminalReplay; over > 0 {
		t.ring = append(t.ring[:0], t.ring[over:]...)
	}
	if t.log != nil && t.written < MaxTerminalLog {
		n, _ := t.log.Write(p)
		t.written += int64(n)
	}
	for c := range t.clients {
		buf := append([]byte(nil), p...)
		select {
		case c.ch <- buf:
		default:
			// Zu langsam: trennen. Der Client verbindet sich neu und bekommt den Puffer.
			delete(t.clients, c)
			close(c.ch)
		}
	}
	return len(p), nil
}

// attach liefert den bisherigen Puffer und einen Kanal für weitere Ausgabe. Der Kanal wird
// geschlossen, wenn der Prozess endet oder der Client nicht hinterherkommt.
func (t *terminal) attach() ([]byte, *termClient) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &termClient{ch: make(chan []byte, termClientBuffer)}
	if t.closed {
		close(c.ch)
	} else {
		t.clients[c] = struct{}{}
	}
	return append([]byte(nil), t.ring...), c
}

func (t *terminal) detach(c *termClient) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.clients[c]; ok {
		delete(t.clients, c)
		close(c.ch)
	}
}

func (t *terminal) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	for c := range t.clients {
		delete(t.clients, c)
		close(c.ch)
	}
	if t.log != nil {
		_ = t.log.Close()
	}
}

// readTail liefert die letzten n Bytes einer Datei (leer, wenn es sie nicht gibt).
func readTail(path string, n int64) []byte {
	f, err := os.Open(path) //nolint:gosec // G304: Pfad baut der Manager aus Datenverzeichnis und Session-ID
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(st.Size()-n, 0)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	return buf
}
