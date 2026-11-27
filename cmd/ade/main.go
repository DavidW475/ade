// Command ade ist die Leitstelle für KI-Agenten: Daemon, CLI und eingebettetes Dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/DavidW475/ade/internal/browser"
	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/daemon"
)

// version wird beim Build per -ldflags "-X main.version=…" gesetzt.
var version = "dev"

const usage = `ade – AI Developer Environment

Verwendung:
  ade <befehl> [optionen]

Befehle:
  start     Daemon starten (falls nötig) und Dashboard im Browser öffnen
  serve     Daemon im Vordergrund ausführen
  project   Projekte verwalten (add, list, remove)
  run       Run einer Software-Fabrik starten (ade run feature --task "…")
  gc        Verwaiste Sandbox-Container entfernen
  proxy     Egress-Proxy der Sandbox (startet der Daemon im Container)
  hook      Freigabe-Hook für Agent-CLIs (bindet der Daemon selbst ein)
  version   Version ausgeben
  help      Diese Hilfe anzeigen

Mit "ade <befehl> -h" werden die Optionen eines Befehls angezeigt.
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "ade:", err)
		}
		os.Exit(exitCode(err))
	}
}

// versionString liefert Version, Commit und Go-Version, z. B.
// "ade v0.1.0 (commit 5933db6, go1.25.4 linux/amd64)".
func versionString() string {
	commit, modified := "unbekannt", false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				commit = s.Value
				if len(commit) > 12 {
					commit = commit[:12]
				}
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	if modified {
		commit += "-dirty"
	}
	return fmt.Sprintf("ade %s (commit %s, %s %s/%s)", version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func exitCode(err error) int {
	var ue usageError
	if errors.As(err, &ue) || errors.Is(err, flag.ErrHelp) {
		return 2
	}
	return 1
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return usageError{"kein Befehl angegeben"}
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "start":
		return cmdStart(rest, stdout, stderr)
	case "serve":
		return cmdServe(rest, stderr)
	case "project":
		return cmdProject(rest, stdout, stderr)
	case "run":
		return cmdRun(rest, stdout, stderr)
	case "gc":
		return cmdGC(rest, stdout, stderr)
	case "proxy":
		return cmdProxy(rest, stderr)
	case "hook":
		return cmdHook(rest, os.Stdin, stdout, stderr)
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, versionString())
		return nil
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return nil
	default:
		_, _ = fmt.Fprint(stderr, usage)
		return usageError{fmt.Sprintf("unbekannter Befehl %q", cmd)}
	}
}

// loadConfig wertet --config aus und liefert Konfiguration und absoluten Pfad.
// Mit allowArgs sind Positionsargumente erlaubt (fs.Args()).
func loadConfig(fs *flag.FlagSet, args []string, allowArgs ...bool) (*config.Config, string, error) {
	def, err := config.DefaultPath()
	if err != nil {
		return nil, "", err
	}
	path := fs.String("config", def, "Pfad zur Konfigurationsdatei")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	if fs.NArg() > 0 && (len(allowArgs) == 0 || !allowArgs[0]) {
		return nil, "", usageError{fmt.Sprintf("unerwartete Argumente: %v", fs.Args())}
	}
	abs, err := filepath.Abs(*path)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load(abs)
	return cfg, abs, err
}

func cmdServe(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	debug := fs.Bool("debug", false, "ausführliches Logging")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return daemon.Run(ctx, cfg, version, log)
}

func cmdStart(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noBrowser := fs.Bool("no-browser", false, "Browser nicht öffnen, nur die Anmelde-URL ausgeben")
	cfg, cfgPath, err := loadConfig(fs, args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, started, err := daemon.EnsureRunning(ctx, cfg, cfgPath)
	if err != nil {
		return err
	}
	if started {
		_, _ = fmt.Fprintf(stdout, "ADE-Daemon gestartet (%s, Log: %s)\n", h.Version, cfg.LogPath())
	} else {
		_, _ = fmt.Fprintf(stdout, "ADE-Daemon läuft bereits (%s)\n", h.Version)
	}

	login, err := daemon.NewClient(cfg).LoginURL(ctx)
	if err != nil {
		return err
	}
	if !*noBrowser {
		err := browser.Open(login)
		if err == nil {
			_, _ = fmt.Fprintf(stdout, "Dashboard geöffnet: %s\n", cfg.BaseURL())
			return nil
		}
		_, _ = fmt.Fprintf(stderr, "Browser konnte nicht geöffnet werden: %v\n", err)
	}
	_, _ = fmt.Fprintf(stdout, "Anmelden (Link gilt einmal, 2 Minuten):\n  %s\n", login)
	return nil
}
