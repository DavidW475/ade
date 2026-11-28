package proxy

// Questions to the daemon (M7, doc/architecture.md#43-approvals).
//
// The proxy container does not reach the daemon (it only listens on the host's 127.0.0.1).
// The proxy therefore holds questions until the daemon fetches them through the control API
// (GET /questions, long polling) and answers them (POST /questions/{id}):
//
//   - tool:   ade hook pretooluse in the agent container asks before a tool call
//     (POST /ade/hook/pretooluse); the answer goes back to the hook unchanged.
//   - egress: a request to a target that is not allowed when the policy sets Ask; the
//     request waits, concurrent requests to the same target share one question.

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

// Kinds of questions.
const (
	QuestionTool   = "tool"
	QuestionEgress = "egress"
)

// Answers.
const (
	AnswerAllow = "allow"
	AnswerDeny  = "deny"
)

// HookPath is the path at which the hook in the agent container asks.
const HookPath = "/ade/hook/pretooluse"

// MaxQuestionWait limits how long a question stays open at most.
const MaxQuestionWait = 2 * time.Hour

// HookCall is the body that ade hook pretooluse sends (locally to the daemon, in the
// sandbox to the proxy).
type HookCall struct {
	// Session is the ADE session (ADE_SESSION_ID).
	Session string `json:"session"`
	// Input is the agent CLI's hook input (unchanged).
	Input json.RawMessage `json:"input"`
}

// Answer is the answer to a question (and to a HookCall).
type Answer struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// Question is an open question to the daemon.
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
	// waiters counts the requests waiting for an egress question.
	waiters int
}

// QuestionsResponse is the answer of GET /questions: all open questions.
type QuestionsResponse struct {
	Boot      string     `json:"boot"`
	Version   int64      `json:"version"`
	Questions []Question `json:"questions"`
}

// questions manages the open questions.
type questions struct {
	mu      sync.Mutex
	open    map[string]*Question
	egress  map[string]*Question // host:port → open question
	denied  map[string]bool      // host:port, denied until the restart
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

// remove takes a question off the list (answered or given up by the asker).
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

// wait waits for the answer; if ctx ends first, the asker gives up (for egress only once
// all waiting requests have given up).
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

// egressQuestion returns the open question for host:port or creates one. denied reports a
// target that was already denied in this run.
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

// askEgress asks the daemon about a target that is not allowed and waits for the answer.
func (s *Server) askEgress(ctx context.Context, host string, port int, limit time.Duration) (bool, string) {
	qu, denied := s.questions.egressQuestion(host, port)
	if denied {
		return false, "denied in the dashboard"
	}
	s.log.Info("approval requested", "host", host, "port", port)
	a, ok := s.questions.wait(ctx, qu, limit)
	if !ok {
		return false, "no decision in the dashboard"
	}
	if a.Decision != AnswerAllow {
		if a.Reason == "" {
			a.Reason = "denied in the dashboard"
		}
		return false, a.Reason
	}
	return true, ""
}

// hook accepts the question of ade hook pretooluse and answers as soon as the daemon has
// decided.
func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var call HookCall
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&call); err != nil || len(call.Input) == 0 {
		writeJSON(w, http.StatusBadRequest, Answer{Decision: AnswerDeny, Reason: "ADE: invalid hook request"})
		return
	}
	qu := &Question{Kind: QuestionTool, Hook: &call, waiters: 1}
	s.questions.add(qu)
	a, ok := s.questions.wait(r.Context(), qu, MaxQuestionWait)
	if !ok {
		writeJSON(w, http.StatusGatewayTimeout, Answer{Decision: AnswerDeny, Reason: "ADE: no answer from the daemon"})
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
		http.Error(w, "answer: decision must be allow or deny", http.StatusBadRequest)
		return
	}
	if !s.questions.answer(r.PathValue("id"), a) {
		http.Error(w, "question not open (any more)", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Questions fetches the open questions; wait > 0 waits that long for a change compared to
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

// Answer answers a question.
func (c *Client) Answer(ctx context.Context, id string, a Answer) error {
	return c.do(ctx, http.MethodPost, "/questions/"+id, a, nil)
}
