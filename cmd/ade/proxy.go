package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/DavidW475/ade/internal/proxy"
)

const proxyUsage = `Verwendung:
  ade proxy [--listen :8080] [--control :8081] [--ide :3000 --ide-target agent:3000]

Egress-Proxy der Sandbox. Läuft im Proxy-Container eines Workspaces und wird vom Daemon
gestartet und gesteuert (doc/sandbox.md); von Hand nur zum Testen. Das Token der Steuer-API
kommt aus der Umgebungsvariable ` + proxy.EnvToken + `.
`

func cmdProxy(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, proxyUsage) }
	listen := fs.String("listen", ":"+strconv.Itoa(proxy.Port), "Adresse für den Agenten")
	control := fs.String("control", ":"+strconv.Itoa(proxy.ControlPort), "Adresse der Steuer-API")
	ide := fs.String("ide", "", "Adresse der IDE-Weiterleitung (leer: keine)")
	ideTarget := fs.String("ide-target", "", "Ziel der IDE-Weiterleitung, z. B. agent:3000")
	debug := fs.Bool("debug", false, "ausführliches Logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("unerwartete Argumente: %v", fs.Args())}
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	token := os.Getenv(proxy.EnvToken)
	if token == "" {
		return fmt.Errorf("%s ist nicht gesetzt", proxy.EnvToken)
	}
	_ = os.Unsetenv(proxy.EnvToken)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return proxy.New(proxy.Options{
		Listen: *listen, Control: *control, IDE: *ide, IDETarget: *ideTarget, Token: token, Logger: log,
	}).Run(ctx)
}
