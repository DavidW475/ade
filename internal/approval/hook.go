package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/proxy"
)

// Environment variables for ade hook pretooluse.
const (
	// EnvHookURL is the address the hook asks (locally the daemon, in the sandbox the egress
	// proxy).
	EnvHookURL = "ADE_HOOK_URL"
	// EnvHookToken authenticates local hooks with the daemon (only valid for the own session).
	EnvHookToken = "ADE_HOOK_TOKEN" //nolint:gosec // G101: name, not a secret
	// EnvSessionID is the ADE session (like agent.EnvSessionID).
	EnvSessionID = "ADE_SESSION_ID"
)

// HookTimeoutMargin is added to the approval timeout so that the CLI does not abort the hook
// before the decision.
const HookTimeoutMargin = 30 * time.Second

// hookOutput is the answer to Claude Code (PreToolUse, JSON on stdout).
type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	} `json:"hookSpecificOutput"`
}

// RunHook is ade hook pretooluse: read the hook input from stdin, ask ADE and write the
// decision in Claude Code's format to stdout. Every error leads to deny (fail closed); the
// return value is only for messages on stderr.
func RunHook(ctx context.Context, stdin io.Reader, stdout io.Writer, getenv func(string) string, client *http.Client) error {
	a, err := askHook(ctx, stdin, getenv, client)
	if err != nil {
		a = proxy.Answer{Decision: proxy.AnswerDeny, Reason: "ADE: approval not possible – " + err.Error()}
	}
	if a.Decision != proxy.AnswerAllow {
		a.Decision = proxy.AnswerDeny
	}
	var out hookOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = a.Decision
	out.HookSpecificOutput.PermissionDecisionReason = a.Reason
	if werr := json.NewEncoder(stdout).Encode(out); werr != nil {
		return werr
	}
	return err
}

func askHook(ctx context.Context, stdin io.Reader, getenv func(string) string, client *http.Client) (proxy.Answer, error) {
	in, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return proxy.Answer{}, fmt.Errorf("read input: %w", err)
	}
	if !json.Valid(in) {
		return proxy.Answer{}, fmt.Errorf("input is not JSON")
	}
	url := getenv(EnvHookURL)
	if !strings.HasPrefix(url, "http://") {
		return proxy.Answer{}, fmt.Errorf("%s is not set", EnvHookURL)
	}
	body, _ := json.Marshal(proxy.HookCall{Session: getenv(EnvSessionID), Input: in})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return proxy.Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t := getenv(EnvHookToken); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if client == nil {
		// Never through HTTP(S)_PROXY: the address is the daemon or the proxy itself.
		client = &http.Client{Transport: &http.Transport{Proxy: nil}}
	}
	resp, err := client.Do(req)
	if err != nil {
		return proxy.Answer{}, fmt.Errorf("ADE not reachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var a proxy.Answer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a); err != nil {
		return proxy.Answer{}, fmt.Errorf("answer from ADE (%d) not readable", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && a.Reason == "" {
		return proxy.Answer{}, fmt.Errorf("ADE answers with %d", resp.StatusCode)
	}
	return a, nil
}

// HookSettings returns the settings for claude --settings: a PreToolUse hook for all tools
// that runs command. timeout is the waiting time for an approval.
func HookSettings(command string, timeout time.Duration) string {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type matcher struct {
		Matcher string `json:"matcher"`
		Hooks   []hook `json:"hooks"`
	}
	s := map[string]map[string][]matcher{"hooks": {"PreToolUse": {{
		Matcher: "*",
		Hooks:   []hook{{Type: "command", Command: command, Timeout: int((timeout + HookTimeoutMargin) / time.Second)}},
	}}}}
	b, _ := json.Marshal(s)
	return string(b)
}

// ShellQuote quotes a path for the hook command (runs through the shell).
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
