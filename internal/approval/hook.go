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

// Umgebungsvariablen für ade hook pretooluse.
const (
	// EnvHookURL ist die Adresse, an die der Hook fragt (lokal der Daemon, in der Sandbox der
	// Egress-Proxy).
	EnvHookURL = "ADE_HOOK_URL"
	// EnvHookToken weist lokale Hooks beim Daemon aus (nur für die eigene Session gültig).
	EnvHookToken = "ADE_HOOK_TOKEN" //nolint:gosec // G101: Name, kein Secret
	// EnvSessionID ist die ADE-Session (wie agent.EnvSessionID).
	EnvSessionID = "ADE_SESSION_ID"
)

// HookTimeoutMargin kommt auf das Timeout der Freigaben, damit die CLI den Hook nicht vor
// der Entscheidung abbricht.
const HookTimeoutMargin = 30 * time.Second

// hookOutput ist die Antwort an Claude Code (PreToolUse, JSON auf stdout).
type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	} `json:"hookSpecificOutput"`
}

// RunHook ist ade hook pretooluse: Hook-Eingabe von stdin lesen, ADE fragen und die
// Entscheidung im Format von Claude Code auf stdout schreiben. Jeder Fehler führt zu deny
// (fail closed); der Rückgabewert ist nur für Meldungen auf stderr.
func RunHook(ctx context.Context, stdin io.Reader, stdout io.Writer, getenv func(string) string, client *http.Client) error {
	a, err := askHook(ctx, stdin, getenv, client)
	if err != nil {
		a = proxy.Answer{Decision: proxy.AnswerDeny, Reason: "ADE: Freigabe nicht möglich – " + err.Error()}
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
		return proxy.Answer{}, fmt.Errorf("Eingabe lesen: %w", err)
	}
	if !json.Valid(in) {
		return proxy.Answer{}, fmt.Errorf("Eingabe ist kein JSON")
	}
	url := getenv(EnvHookURL)
	if !strings.HasPrefix(url, "http://") {
		return proxy.Answer{}, fmt.Errorf("%s ist nicht gesetzt", EnvHookURL)
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
		// Nie über HTTP(S)_PROXY: Die Adresse ist der Daemon bzw. der Proxy selbst.
		client = &http.Client{Transport: &http.Transport{Proxy: nil}}
	}
	resp, err := client.Do(req)
	if err != nil {
		return proxy.Answer{}, fmt.Errorf("ADE nicht erreichbar: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var a proxy.Answer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a); err != nil {
		return proxy.Answer{}, fmt.Errorf("Antwort von ADE (%d) nicht lesbar", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && a.Reason == "" {
		return proxy.Answer{}, fmt.Errorf("ADE antwortet mit %d", resp.StatusCode)
	}
	return a, nil
}

// HookSettings liefert die Einstellungen für claude --settings: ein PreToolUse-Hook für alle
// Werkzeuge, der command ausführt. timeout ist die Wartezeit auf eine Freigabe.
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

// ShellQuote setzt einen Pfad für den Hook-Befehl (läuft über die Shell) in Anführungszeichen.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
