// Package service richtet den Daemon als systemd-User-Dienst ein (`ade install-service`).
//
// Die Unit startet `ade serve` mit einer festen Konfiguration. Weil systemd-User-Dienste die
// Umgebung der Shell nicht erben, übernimmt die Unit den aktuellen PATH (git, docker, gh,
// claude, codex). Secrets gehören nicht in die Unit, sondern in die Konfiguration.
package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Name ist der Name der Unit.
const Name = "ade.service"

// configMarker steht als Kommentar in der Unit und nennt die Konfiguration, mit der sie startet.
const configMarker = "# ade-config: "

// ErrNoSystemd bedeutet, dass kein systemd-User-Manager erreichbar ist.
var ErrNoSystemd = errors.New("systemd ist nicht verfügbar")

// Runner führt systemctl aus und liefert die kombinierte Ausgabe.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Manager installiert und entfernt die Unit.
type Manager struct {
	// UnitDir ist das Verzeichnis der User-Units (Standard: $XDG_CONFIG_HOME/systemd/user).
	UnitDir string
	// Exe ist der absolute Pfad des ade-Binarys.
	Exe string
	// ConfigPath ist der absolute Pfad der Konfiguration.
	ConfigPath string
	// LogPath ist das Daemon-Log; dorthin gehen stdout und stderr.
	LogPath string
	// Path ist der PATH des Dienstes.
	Path string
	// Run führt systemctl aus (Standard: echtes systemctl).
	Run Runner
}

// New erzeugt einen Manager mit Standardwerten für den aktuellen Benutzer.
func New(configPath, logPath string) (*Manager, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	dir, err := DefaultUnitDir()
	if err != nil {
		return nil, err
	}
	return &Manager{
		UnitDir:    dir,
		Exe:        exe,
		ConfigPath: configPath,
		LogPath:    logPath,
		Path:       os.Getenv("PATH"),
		Run:        systemctl,
	}, nil
}

// DefaultUnitDir liefert $XDG_CONFIG_HOME/systemd/user bzw. ~/.config/systemd/user.
func DefaultUnitDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user"), nil
}

func systemctl(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
}

// UnitPath ist der Pfad der Unit-Datei.
func (m *Manager) UnitPath() string { return filepath.Join(m.UnitDir, Name) }

// Unit liefert den Inhalt der Unit-Datei.
func (m *Manager) Unit() string {
	var b strings.Builder
	b.WriteString("# Erzeugt von `ade install-service`; entfernen mit `ade uninstall-service`.\n")
	b.WriteString(configMarker + m.ConfigPath + "\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=ADE – Leitstelle für KI-Agenten\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s serve --config %s\n", quote(m.Exe), quote(m.ConfigPath))
	if m.Path != "" {
		// In Environment= ersetzt systemd keine Variablen, nur Spezifizierer.
		fmt.Fprintf(&b, "Environment=%s\n", quoteEnv("PATH="+m.Path))
	}
	if m.LogPath != "" {
		// append: gibt es seit systemd 240 (Ubuntu ab 20.04).
		fmt.Fprintf(&b, "StandardOutput=append:%s\n", escape(m.LogPath))
		fmt.Fprintf(&b, "StandardError=append:%s\n", escape(m.LogPath))
	}
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// escape maskiert Spezifizierer (%) und Variablen ($) für systemd.
func escape(s string) string {
	return strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
}

// quote setzt einen Wert für ExecStart/Environment in doppelte Anführungszeichen.
func quote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(escape(s))
	return `"` + s + `"`
}

// quoteEnv setzt einen Wert für Environment in doppelte Anführungszeichen.
func quoteEnv(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(s)
	return `"` + s + `"`
}

// Available prüft, ob der systemd-User-Manager läuft.
func (m *Manager) Available(ctx context.Context) error {
	out, err := m.Run(ctx, "show-environment")
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%w (%s).\nUnter WSL in /etc/wsl.conf eintragen:\n  [boot]\n  systemd=true\n"+
			"dann in Windows `wsl --shutdown` ausführen und die WSL neu öffnen", ErrNoSystemd, firstLine(msg))
	}
	return nil
}

// Installed meldet, ob die Unit-Datei existiert.
func (m *Manager) Installed() bool {
	_, err := os.Stat(m.UnitPath())
	return err == nil
}

// InstalledConfig liefert die Konfiguration, mit der die installierte Unit startet ("" ohne Unit).
func (m *Manager) InstalledConfig() string {
	data, err := os.ReadFile(m.UnitPath())
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), configMarker); ok {
			return v
		}
	}
	return ""
}

// Active meldet, ob der Dienst läuft.
func (m *Manager) Active(ctx context.Context) bool {
	out, err := m.Run(ctx, "is-active", Name)
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// Install schreibt die Unit, lädt systemd neu und aktiviert und startet den Dienst.
func (m *Manager) Install(ctx context.Context) error {
	if err := m.Available(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(m.UnitDir, 0o700); err != nil {
		return err
	}
	tmp := m.UnitPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(m.Unit()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.UnitPath()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := m.call(ctx, "daemon-reload"); err != nil {
		return err
	}
	// restart statt start: Eine geänderte Unit (neues Binary, andere Konfiguration) wirkt sofort.
	if err := m.call(ctx, "enable", Name); err != nil {
		return err
	}
	return m.call(ctx, "restart", Name)
}

// Uninstall stoppt und deaktiviert den Dienst und entfernt die Unit.
func (m *Manager) Uninstall(ctx context.Context) error {
	if !m.Installed() {
		return fmt.Errorf("%s ist nicht installiert (%s)", Name, m.UnitPath())
	}
	if err := m.Available(ctx); err == nil {
		if err := m.call(ctx, "disable", "--now", Name); err != nil {
			return err
		}
	}
	if err := os.Remove(m.UnitPath()); err != nil {
		return err
	}
	if err := m.Available(ctx); err == nil {
		return m.call(ctx, "daemon-reload")
	}
	return nil
}

// Start startet den installierten Dienst.
func (m *Manager) Start(ctx context.Context) error { return m.call(ctx, "start", Name) }

// Linger meldet, ob für den Benutzer Lingering aktiv ist (der User-Manager läuft dann ohne
// offene Sitzung). ok ist false, wenn sich das nicht feststellen lässt.
func Linger(ctx context.Context) (enabled, ok bool) {
	out, err := exec.CommandContext(ctx, "loginctl", "show-user", fmt.Sprint(os.Getuid()), "--property=Linger", "--value").Output()
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(out)) == "yes", true
}

func (m *Manager) call(ctx context.Context, args ...string) error {
	out, err := m.Run(ctx, args...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
