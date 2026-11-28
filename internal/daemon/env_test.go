package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/sandbox"
)

func oauthConfig(t *testing.T) *config.Config {
	t.Helper()
	token := filepath.Join(t.TempDir(), "claude-oauth-token")
	if err := os.WriteFile(token, []byte("sk-ant-oat01-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Providers: map[string]config.Provider{"anthropic": {OAuthToken: "file:" + token}}}
}

func TestOAuthTokenLocal(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := adapterEnv(oauthConfig(t), log)("claude")
	if !slices.Equal(env, []string{"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-secret"}) {
		t.Fatalf("claude env = %q", env)
	}
	if env := adapterEnv(oauthConfig(t), log)("codex"); env != nil {
		t.Fatalf("codex env = %q", env)
	}
}

func TestOAuthTokenSandbox(t *testing.T) {
	cfg := oauthConfig(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	env := sandboxAdapterEnv(cfg)("claude")
	want := []string{
		"ANTHROPIC_BASE_URL=" + sandbox.ProviderBaseURL("anthropic"),
		"CLAUDE_CODE_OAUTH_TOKEN=" + KeyPlaceholder,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	if !slices.Equal(env, want) {
		t.Fatalf("sandbox env = %q, want %q", env, want)
	}
	// The real token never enters the container environment.
	for _, e := range env {
		if strings.Contains(e, "sk-ant-oat01-secret") || strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			t.Fatalf("container env leaks credentials: %q", e)
		}
	}

	if s := sandboxSecrets(cfg, log)(); s["anthropic"] != "sk-ant-oat01-secret" {
		t.Fatalf("proxy secrets = %v", s)
	}
	if _, ok := providerAccess(cfg)("claude", true); !ok {
		t.Fatal("providerAccess: proxy does not count usage for an OAuth token")
	}
}

func TestAPIKeySandboxUnchanged(t *testing.T) {
	t.Setenv("ADE_TEST_ANTHROPIC", "sk-ant-api03-x")
	cfg := &config.Config{Providers: map[string]config.Provider{"anthropic": {APIKey: "env:ADE_TEST_ANTHROPIC"}}}
	env := sandboxAdapterEnv(cfg)("claude")
	if !slices.Contains(env, "ANTHROPIC_API_KEY="+KeyPlaceholder) || slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") }) {
		t.Fatalf("sandbox env = %q", env)
	}
}
