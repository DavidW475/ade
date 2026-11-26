package agent

import (
	"os"
	"slices"
	"strings"
)

// passEnv sind die Variablen der Daemon-Umgebung, die ein Agent-Prozess erbt. Alles andere
// (Tokens wie GH_TOKEN, SSH_AUTH_SOCK, Cloud-Zugänge, ADE_*-Variablen des Daemons) bleibt
// draußen – der Agent soll nichts veröffentlichen können (doc/architecture.md#7-sicherheit).
var passEnv = []string{
	"HOME", "USER", "LOGNAME", "SHELL", "PATH", "LANG", "LANGUAGE", "TZ", "TMPDIR",
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
	// WSL: Windows-Programme starten (explorer.exe, wslview) und Pfade übersetzen.
	"WSL_DISTRO_NAME", "WSL_INTEROP", "WSLENV",
	"DISPLAY", "WAYLAND_DISPLAY",
}

// Variablen, die ADE jedem Prozess setzt.
const (
	EnvSessionID     = "ADE_SESSION_ID"
	EnvWorkspaceID   = "ADE_WORKSPACE_ID"
	EnvWorkspacePath = "ADE_WORKSPACE_PATH"
)

// FilterEnv liefert die Variablen aus base (KEY=VALUE), die ADE an Prozesse in einem
// Workspace weitergibt (passEnv und LC_*). Die Browser-IDE nutzt dieselbe Liste.
func FilterEnv(base []string) []string {
	var out []string
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok && (slices.Contains(passEnv, k) || strings.HasPrefix(k, "LC_")) {
			out = append(out, kv)
		}
	}
	return out
}

// buildEnv baut die kontrollierte Umgebung eines Agent-Prozesses aus base (KEY=VALUE),
// den ADE-Variablen und den Variablen des Adapters (extra überschreibt alles davor).
func buildEnv(base []string, terminal bool, sessionID, workspaceID, workdir string, extra []string) []string {
	vars := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := vars[k]; !ok {
			order = append(order, k)
		}
		vars[k] = v
	}
	for _, kv := range FilterEnv(base) {
		k, v, _ := strings.Cut(kv, "=")
		set(k, v)
	}
	if terminal {
		set("TERM", "xterm-256color")
		set("COLORTERM", "truecolor")
	} else {
		set("TERM", "dumb")
		set("NO_COLOR", "1")
	}
	set(EnvSessionID, sessionID)
	set(EnvWorkspaceID, workspaceID)
	set(EnvWorkspacePath, workdir)
	set("PWD", workdir)
	for _, kv := range extra {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+vars[k])
	}
	return out
}

func daemonEnv() []string { return os.Environ() }
