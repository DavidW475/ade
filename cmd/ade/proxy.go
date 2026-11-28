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

const proxyUsage = `Usage:
  ade proxy [--listen :8080] [--control :8081] [--ide :3000 --ide-target agent:3000]

Egress proxy of the sandbox. It runs in a workspace's proxy container and is started and
controlled by the daemon (doc/sandbox.md); by hand only for testing. The control API's token
comes from the environment variable ` + proxy.EnvToken + `.
`

func cmdProxy(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, proxyUsage) }
	listen := fs.String("listen", ":"+strconv.Itoa(proxy.Port), "address for the agent")
	control := fs.String("control", ":"+strconv.Itoa(proxy.ControlPort), "address of the control API")
	ide := fs.String("ide", "", "address of the IDE forwarding (empty: none)")
	ideTarget := fs.String("ide-target", "", "target of the IDE forwarding, e.g. agent:3000")
	debug := fs.Bool("debug", false, "verbose logging")
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
		return fmt.Errorf("%s is not set", proxy.EnvToken)
	}
	_ = os.Unsetenv(proxy.EnvToken)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return proxy.New(proxy.Options{
		Listen: *listen, Control: *control, IDE: *ide, IDETarget: *ideTarget, Token: token, Logger: log,
	}).Run(ctx)
}
