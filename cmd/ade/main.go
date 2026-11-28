// Command ade is the control center for AI agents: daemon, CLI and embedded dashboard.
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

// version is set at build time via -ldflags "-X main.version=…".
var version = "dev"

const usage = `ade – AI Developer Environment

Usage:
  ade <command> [options]

Commands:
  start              start the daemon (if needed) and open the dashboard in the browser
  serve              run the daemon in the foreground
  project            manage projects (add, list, remove)
  run                start a run of a software factory (ade run feature --task "…")
  gc                 remove orphaned sandbox containers
  install-service    set up and start the daemon as a systemd user service
  uninstall-service  stop and remove the systemd user service
  proxy              egress proxy of the sandbox (the daemon starts it in the container)
  hook               approval hook for agent CLIs (the daemon registers it itself)
  version            print the version
  help               show this help

"ade <command> -h" shows the options of a command.
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "ade:", err)
		}
		os.Exit(exitCode(err))
	}
}

// versionString returns version, commit and Go version, e.g.
// "ade v0.1.0 (commit 5933db6, go1.25.4 linux/amd64)".
func versionString() string {
	commit, modified := "unknown", false
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
		return usageError{"no command given"}
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
	case "install-service":
		return cmdInstallService(rest, stdout, stderr)
	case "uninstall-service":
		return cmdUninstallService(rest, stdout, stderr)
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
		return usageError{fmt.Sprintf("unknown command %q", cmd)}
	}
}

// loadConfig evaluates --config and returns configuration and absolute path.
// With allowArgs positional arguments are allowed (fs.Args()).
func loadConfig(fs *flag.FlagSet, args []string, allowArgs ...bool) (*config.Config, string, error) {
	def, err := config.DefaultPath()
	if err != nil {
		return nil, "", err
	}
	path := fs.String("config", def, "path to the configuration file")
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
	debug := fs.Bool("debug", false, "verbose logging")
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
	noBrowser := fs.Bool("no-browser", false, "do not open the browser, only print the login URL")
	cfg, cfgPath, err := loadConfig(fs, args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, how, err := ensureRunning(ctx, cfg, cfgPath)
	if err != nil {
		return err
	}
	switch how {
	case "systemd":
		_, _ = fmt.Fprintf(stdout, "ADE service started (%s, log: %s)\n", h.Version, cfg.LogPath())
	case "process":
		_, _ = fmt.Fprintf(stdout, "ADE daemon started (%s, log: %s)\n", h.Version, cfg.LogPath())
	default:
		_, _ = fmt.Fprintf(stdout, "ADE daemon is already running (%s)\n", h.Version)
	}

	login, err := daemon.NewClient(cfg).LoginURL(ctx)
	if err != nil {
		return err
	}
	if !*noBrowser {
		err := browser.Open(login)
		if err == nil {
			_, _ = fmt.Fprintf(stdout, "dashboard opened: %s\n", cfg.BaseURL())
			return nil
		}
		_, _ = fmt.Fprintf(stderr, "browser could not be opened: %v\n", err)
	}
	_, _ = fmt.Fprintf(stdout, "Log in (the link works once, for 2 minutes):\n  %s\n", login)
	return nil
}
