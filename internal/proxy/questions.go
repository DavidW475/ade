package proxy

// Rückfragen an den Daemon (M7, doc/architecture.md#43-freigaben).
//
// Der Proxy-Container erreicht den Daemon nicht (er lauscht nur auf 127.0.0.1 des Hosts).
// Deshalb hält der Proxy Fragen fest, bis der Daemon sie über die Steuer-API abholt
// (GET /questions, Long-Polling) und beantwortet (POST /questions/{id}):
//
//   - tool:   ade hook pretooluse im Agent-Container fragt vor einem Werkzeug-Aufruf
//     (POST /ade/hook/pretooluse); die Antwort geht unverändert an den Hook zurück.
//   - egress: eine Anfrage an ein nicht freigegebenes Ziel, wenn die Policy Ask setzt; die
//     Anfrage wartet, gleichzeitige Anfragen an dasselbe Ziel teilen sich eine Frage.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Arten von Fragen.
const (
	QuestionTool   = "tool"
	QuestionEgress = "egress"
)

// Antworten.
const (
	AnswerAllow = "allow"
	AnswerDeny  = "deny"
)

// HookPath ist der Pfad, unter dem der Hook im Agent-Container fragt.
const HookPath = "/ade/hook/pretooluse"

// MaxQuestionWait begrenzt, wie lange eine Frage höchstens offen bleibt.
const MaxQuestionWait = 2 * time.Hour

// HookCall ist der Body, den ade hook pretooluse schickt (lokal an den Daemon, in der
// Sandbox an den Proxy).
type HookCall struct {
	// Session ist die ADE-Session (ADE_SESSION_ID).
	Session string `json:"session"`
	// Input ist die Hook-Eingabe der Agent-CLI (unverändert).
	Input json.RawMessage `json:"input"`
}

// Answer ist die Antwort auf eine Frage (und auf einen HookCall).
type Answer struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// Question ist eine offene Frage an den Daemon.
type Question struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Created time.Time `json:"created"`
	// tool
	Hook *HookCall `json:"hook,omitempty"`
	// egress
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`

	done   chan struct{}
	answer Answer
	// waiters zählt die Anfragen, die auf eine egress-Frage warten.
	waiters int
}

// QuestionsResponse ist die Antwort von GET /questions: alle offenen Fragen.
type QuestionsResponse struct {
	Boot      string     `json:"boot"`
	Version   int64      `json:"version"`
	Questions []Question `json:"questions"`
}

// questions verwaltet die offenen Fragen.
type questions struct {
	mu      sync.Mutex
	open    map[string]*Question
	egress  map[string]*Question // host:port → offene Frage
	denied  map[string]bool      // host:port, abgelehnt bis zum Neustart
	version int64
	notify  chan struct{}
}

func (q *questions) init() {
	q.open = map[string]*Question{}
	q.egress = map[string]*Question{}
	q.denied = map[string]bool{}
	q.notify = make(chan struct{})
}

func (q *questions) changedLocked() {
	q.version++
	close(q.notify)
	q.notify = make(chan struct{})
}

func (q *questions) add(qu *Question) {
	q.mu.Lock()
	defer q.mu.Unlock()
	qu.ID, qu.Created, qu.done = randomID(), time.Now().UTC(), make(chan struct{})
	q.open[qu.ID] = qu
	q.changedLocked()
}

// remove nimmt eine Frage aus der Liste (beantwortet oder vom Fragenden aufgegeben).
func (q *questions) removeLocked(qu *Question) {
	if q.open[qu.ID] != qu {
		return
	}
	delete(q.open, qu.ID)
	if qu.Kind == QuestionEgress {
		k := net.JoinHostPort(qu.Host, strconv.Itoa(qu.Port))
		if q.egress[k] == qu {
			delete(q.egress, k)
		}
	}
	q.changedLocked()
}

func (q *questions) answer(id string, a Answer) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	qu := q.open[id]
	if qu == nil {
		return false
	}
	qu.answer = a
	if qu.Kind == QuestionEgress && a.Decision != AnswerAllow {
		q.denied[net.JoinHostPort(qu.Host, strconv.Itoa(qu.Port))] = true
	}
	close(qu.done)
	q.removeLocked(qu)
	return true
}

func (q *questions) list() ([]Question, int64, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Question, 0, len(q.open))
	for _, qu := range q.open {
		c := *qu
		c.done = nil
		out = append(out, c)
	}
	return out, q.version, q.notify
}

// wait wartet auf die Antwort; endet ctx vorher, gibt der Fragende auf (bei egress erst,
// wenn alle Wartenden aufgegeben haben).
func (q *questions) wait(ctx context.Context, qu *Question, limit time.Duration) (Answer, bool) {
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-qu.done:
		return qu.answer, true
	case <-ctx.Done():
	case <-t.C:
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	select {
	case <-qu.done:
		return qu.answer, true
	default:
	}
	qu.waiters--
	if qu.waiters <= 0 {
		q.removeLocked(qu)
	}
	return Answer{}, false
}

// egressQuestion liefert die offene Frage für host:port oder legt eine an. denied meldet ein
// in diesem Lauf bereits abgelehntes Ziel.
func (q *questions) egressQuestion(host string, port int) (qu *Question, denied bool) {
	k := net.JoinHostPort(host, strconv.Itoa(port))
	q.mu.Lock()
	if q.denied[k] {
		q.mu.Unlock()
		return nil, true
	}
	if qu = q.egress[k]; qu != nil {
		qu.waiters++
		q.mu.Unlock()
		return qu, false
	}
	qu = &Question{Kind: QuestionEgress, Host: host, Port: port, waiters: 1,
		ID: randomID(), Created: time.Now().UTC(), done: make(chan struct{})}
	q.open[qu.ID] = qu
	q.egress[k] = qu
	q.changedLocked()
	q.mu.Unlock()
	return qu, false
}

// askEgress fragt den Daemon nach einem nicht freigegebenen Ziel und wartet auf die Antwort.
func (s *Server) askEgress(ctx context.Context, host string, port int, limit time.Duration) (bool, string) {
	qu, denied := s.questions.egressQuestion(host, port)
	if denied {
		return false, "im Dashboard abgelehnt"
	}
	s.log.Info("Freigabe angefragt", "host", host, "port", port)
	a, ok := s.questions.wait(ctx, qu, limit)
	if !ok {
		return false, "keine Entscheidung im Dashboard"
	}
	if a.Decision != AnswerAllow {
		if a.Reason == "" {
			a.Reason = "im Dashboard abgelehnt"
		}
		return false, a.Reason
	}
	return true, ""
}

// hook nimmt die Frage von ade hook pretooluse entgegen und antwortet, sobald der Daemon
// entschieden hat.
func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "nur POST", http.StatusMethodNotAllowed)
		return
	}
	var call HookCall
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&call); err != nil || len(call.Input) == 0 {
		writeJSON(w, http.StatusBadRequest, Answer{Decision: AnswerDeny, Reason: "ADE: ungültige Hook-Anfrage"})
		return
	}
	qu := &Question{Kind: QuestionTool, Hook: &call, waiters: 1}
	s.questions.add(qu)
	a, ok := s.questions.wait(r.Context(), qu, MaxQuestionWait)
	if !ok {
		writeJSON(w, http.StatusGatewayTimeout, Answer{Decision: AnswerDeny, Reason: "ADE: keine Antwort vom Daemon"})
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleQuestions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	version, _ := strconv.ParseInt(q.Get("version"), 10, 64)
	wait, _ := time.ParseDuration(q.Get("wait"))
	wait = min(max(wait, 0), MaxEventWait)
	list, v, ch := s.questions.list()
	if (q.Get("boot") == "" || q.Get("boot") == s.boot) && v == version && wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ch:
		case <-t.C:
		case <-r.Context().Done():
		}
		t.Stop()
		list, v, _ = s.questions.list()
	}
	writeJSON(w, http.StatusOK, QuestionsResponse{Boot: s.boot, Version: v, Questions: list})
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var a Answer
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&a); err != nil ||
		(a.Decision != AnswerAllow && a.Decision != AnswerDeny) {
		http.Error(w, "Antwort: decision muss allow oder deny sein", http.StatusBadRequest)
		return
	}
	if !s.questions.answer(r.PathValue("id"), a) {
		http.Error(w, "Frage nicht (mehr) offen", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Questions holt die offenen Fragen; wait > 0 wartet so lange auf eine Änderung gegenüber
// version.
func (c *Client) Questions(ctx context.Context, boot string, version int64, wait time.Duration) (QuestionsResponse, error) {
	var r QuestionsResponse
	path := "/questions?version=" + strconv.FormatInt(version, 10) + "&wait=" + wait.String()
	if boot != "" {
		path += "&boot=" + boot
	}
	err := c.do(ctx, http.MethodGet, path, nil, &r)
	return r, err
}

// Answer beantwortet eine Frage.
func (c *Client) Answer(ctx context.Context, id string, a Answer) error {
	return c.do(ctx, http.MethodPost, "/questions/"+id, a, nil)
}
