package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DavidW475/ade/internal/proxy"
)

func runHook(t *testing.T, input string, env map[string]string) (decision, reason string) {
	t.Helper()
	var out bytes.Buffer
	_ = RunHook(context.Background(), strings.NewReader(input), &out, func(k string) string { return env[k] }, nil)
	var o hookOutput
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	if o.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("output %q", out.String())
	}
	return o.HookSpecificOutput.PermissionDecision, o.HookSpecificOutput.PermissionDecisionReason
}

func TestRunHook(t *testing.T) {
	var got proxy.HookCall
	var auth string
	answer := proxy.Answer{Decision: proxy.AnswerAllow, Reason: "ok"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(answer)
	}))
	defer srv.Close()
	env := map[string]string{EnvHookURL: srv.URL, EnvHookToken: "tok", EnvSessionID: "s1"}

	d, r := runHook(t, `{"tool_name":"Bash","tool_input":{"command":"ls"}}`, env)
	if d != "allow" || r != "ok" || auth != "Bearer tok" || got.Session != "s1" || !strings.Contains(string(got.Input), `"Bash"`) {
		t.Fatalf("%s %q, auth %q, call %+v", d, r, auth, got)
	}
	answer = proxy.Answer{Decision: "irgendwas"}
	if d, _ := runHook(t, `{}`, env); d != "deny" {
		t.Fatalf("unknown answer: %s", d)
	}

	// Errors always lead to deny.
	for name, c := range map[string]struct {
		in  string
		env map[string]string
	}{
		"not JSON":      {"nope", env},
		"without URL":   {`{}`, map[string]string{}},
		"not reachable": {`{}`, map[string]string{EnvHookURL: "http://127.0.0.1:1/x"}},
	} {
		if d, r := runHook(t, c.in, c.env); d != "deny" || !strings.HasPrefix(r, "ADE:") {
			t.Errorf("%s: %s %q", name, d, r)
		}
	}
}

func TestHookSettingsQuote(t *testing.T) {
	if got := ShellQuote("/a b/it's"); got != `'/a b/it'\''s'` {
		t.Fatal(got)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(HookSettings("x", 0)), &v); err != nil {
		t.Fatal(err)
	}
}
