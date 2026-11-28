package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// waitQuestion waits until the proxy has an open question and returns it.
func waitQuestion(t *testing.T, c *Client, kind string) Question {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var version int64
	for time.Now().Before(deadline) {
		r, err := c.Questions(context.Background(), "", version, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		version = r.Version
		for _, q := range r.Questions {
			if q.Kind == kind {
				return q
			}
		}
	}
	t.Fatalf("no question of kind %s", kind)
	return Question{}
}

func TestEgressAskAllow(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer backend.Close()
	s, ts := testProxy(t, backend.Listener.Addr().String(), Options{Token: "t"})
	ctl := httptest.NewServer(s.ControlHandler())
	defer ctl.Close()
	c := &Client{Base: ctl.URL, Token: "t"}
	if err := s.SetPolicy(Policy{Allow: []string{"allowed.example"}, Deny: []string{"bad.example"}, Ask: true}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status int
		body   string
	}
	get := func(u string) <-chan result {
		ch := make(chan result, 1)
		go func() {
			resp, err := proxyClient(ts).Get(u)
			if err != nil {
				ch <- result{status: -1, body: err.Error()}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			ch <- result{resp.StatusCode, string(b)}
		}()
		return ch
	}

	// Two concurrent requests to the same target share one question.
	r1, r2 := get("http://new.example/a"), get("http://new.example/b")
	q := waitQuestion(t, c, QuestionEgress)
	if q.Host != "new.example" || q.Port != 80 {
		t.Fatalf("question %+v", q)
	}
	time.Sleep(100 * time.Millisecond)
	if r, _ := c.Questions(context.Background(), "", 0, 0); len(r.Questions) != 1 {
		t.Fatalf("%d questions, want 1", len(r.Questions))
	}
	if err := c.Answer(context.Background(), q.ID, Answer{Decision: AnswerAllow}); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan result{r1, r2} {
		if r := <-ch; r.status != 200 || r.body != "ok" {
			t.Fatalf("answer %+v", r)
		}
	}
	if ev := lastEvent(t, s); !ev.Allowed || ev.Reason != ReasonApproved {
		t.Fatalf("Event %+v", ev)
	}
	if err := c.Answer(context.Background(), q.ID, Answer{Decision: AnswerAllow}); err == nil {
		t.Fatal("second answer accepted")
	}

	// The deny list never asks.
	if r := <-get("http://bad.example/"); r.status != http.StatusForbidden {
		t.Fatalf("deny list: %+v", r)
	}
	if r, _ := c.Questions(context.Background(), "", 0, 0); len(r.Questions) != 0 {
		t.Fatalf("question for the deny list: %+v", r.Questions)
	}

	// Deny: this target is not asked again until the restart.
	r3 := get("http://other.example/")
	q = waitQuestion(t, c, QuestionEgress)
	if err := c.Answer(context.Background(), q.ID, Answer{Decision: AnswerDeny, Reason: "nein"}); err != nil {
		t.Fatal(err)
	}
	if r := <-r3; r.status != http.StatusForbidden || !strings.Contains(r.body, "nein") {
		t.Fatalf("denied: %+v", r)
	}
	start := time.Now()
	if r := <-get("http://other.example/"); r.status != http.StatusForbidden || time.Since(start) > 2*time.Second {
		t.Fatalf("asked again: %+v", r)
	}
}

func TestEgressAskTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	s, ts := testProxy(t, backend.Listener.Addr().String(), Options{})
	if err := s.SetPolicy(Policy{Ask: true, AskWaitSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	resp, err := proxyClient(ts).Get("http://slow.example/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Status %d", resp.StatusCode)
	}
	if qs, _, _ := s.questions.list(); len(qs) != 0 {
		t.Fatalf("question still open after expiry: %+v", qs)
	}
}

func TestHookRelay(t *testing.T) {
	s, ts := testProxy(t, "127.0.0.1:1", Options{Token: "t"})
	ctl := httptest.NewServer(s.ControlHandler())
	defer ctl.Close()
	c := &Client{Base: ctl.URL, Token: "t"}

	call := HookCall{Session: "s1", Input: json.RawMessage(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)}
	body, _ := json.Marshal(call)
	done := make(chan Answer, 1)
	go func() {
		// Like the hook in the container: directly to the proxy, without HTTP_PROXY.
		resp, err := http.Post(ts.URL+HookPath, "application/json", bytes.NewReader(body))
		if err != nil {
			done <- Answer{Reason: err.Error()}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var a Answer
		_ = json.NewDecoder(resp.Body).Decode(&a)
		done <- a
	}()
	q := waitQuestion(t, c, QuestionTool)
	if q.Hook == nil || q.Hook.Session != "s1" || !strings.Contains(string(q.Hook.Input), "Bash") {
		t.Fatalf("question %+v", q)
	}
	if err := c.Answer(context.Background(), q.ID, Answer{Decision: AnswerDeny, Reason: "rather not"}); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.Decision != AnswerDeny || a.Reason != "rather not" {
		t.Fatalf("answer %+v", a)
	}

	// If the hook gives up, the question disappears.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+HookPath, bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	waitQuestion(t, c, QuestionTool)
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if r, _ := c.Questions(context.Background(), "", 0, 0); len(r.Questions) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("question still open after aborting")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Invalid answers and control API without a token.
	if err := c.Answer(context.Background(), "gibtsnicht", Answer{Decision: "vielleicht"}); err == nil {
		t.Fatal("invalid answer accepted")
	}
	if _, err := (&Client{Base: ctl.URL, Token: "falsch"}).Questions(context.Background(), "", 0, 0); !IsUnauthorized(err) {
		t.Fatalf("without token: %v", err)
	}
}
