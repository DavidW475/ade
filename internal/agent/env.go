package agent

import (
	"os"
	"slices"
	"strings"
)

// passEnv are the variables of the daemon's environment an agent process inherits. Everything
// else (tokens like GH_TOKEN, SSH_AUTH_SOCK, cloud credentials, the daemon's ADE_* variables)
// stays out – the agent must not be able to publish anything (doc/architecture.md#7-security).
var passEnv = []string{
	"HOME", "USER", "LOGNAME", "SHELL", "PATH", "LANG", "LANGUAGE", "TZ", "TMPDIR",
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
	// WSL: start Windows programs (explorer.exe, wslview) and translate paths.
	"WSL_DISTRO_NAME", "WSL_INTEROP", "WSLENV",
	"DISPLAY", "WAYLAND_DISPLAY",
}

// Variables ADE sets for every process.
const (
	EnvSessionID     = "ADE_SESSION_ID"
	EnvWorkspaceID   = "ADE_WORKSPACE_ID"
	EnvWorkspacePath = "ADE_WORKSPACE_PATH"
)

// FilterEnv returns the variables from base (KEY=VALUE) that ADE passes to processes in a
// workspace (passEnv and LC_*). The browser IDE uses the same list.
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

// buildEnv builds the controlled environment of an agent process from base (KEY=VALUE), the
// ADE variables and the adapter's variables (extra overrides everything before it).
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

// sandboxEnv is the environment of an agent process in the container: nothing from the host,
// only terminal settings, ADE variables and the adapter's variables. HOME, PATH and LANG come
// from the image.
func sandboxEnv(terminal bool, sessionID, workspaceID string, extra []string) []string {
	env := []string{"TERM=dumb", "NO_COLOR=1"}
	if terminal {
		env = []string{"TERM=xterm-256color", "COLORTERM=truecolor"}
	}
	env = append(env, EnvSessionID+"="+sessionID, EnvWorkspaceID+"="+workspaceID, EnvWorkspacePath+"=/workspace")
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "HOME" || k == "PATH" || k == EnvSessionID {
			continue
		}
		env = append(env, kv)
	}
	return env
}
