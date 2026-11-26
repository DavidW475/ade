package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/DavidW475/ade/internal/api"
	"github.com/DavidW475/ade/internal/daemon"
	"github.com/DavidW475/ade/internal/sandbox"
)

const gcUsage = `Verwendung:
  ade gc [--dry-run]

Entfernt verwaiste Sandbox-Container: Container dieser ADE-Instanz ohne Workspace und
Container anderer Instanzen, deren Datenverzeichnis nicht mehr existiert. Container
bestehender Workspaces bleiben stehen. Läuft der Daemon, erledigt er das selbst.
`

func cmdGC(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, gcUsage) }
	dry := fs.Bool("dry-run", false, "nur auflisten, nichts entfernen")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var removed []sandbox.Container
	c := daemon.NewClient(cfg)
	if _, err := c.Health(ctx); err == nil {
		p := "/api/sandbox/gc"
		if *dry {
			p += "?dry_run=1"
		}
		var resp api.GCResponse
		if err := c.Do(ctx, "POST", p, nil, &resp); err != nil {
			return err
		}
		removed = resp.Removed
	} else if errors.Is(err, daemon.ErrNotRunning) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		if removed, err = daemon.GC(ctx, cfg, version, *dry, log); err != nil {
			return err
		}
	} else {
		return err
	}

	verb := "entfernt"
	if *dry {
		verb = "würde entfernt"
	}
	if len(removed) == 0 {
		_, _ = fmt.Fprintln(stdout, "Keine verwaisten Sandbox-Container.")
		return nil
	}
	for _, ct := range removed {
		_, _ = fmt.Fprintf(stdout, "%s %s (Workspace %s, %s)\n", verb, ct.Name, ct.Workspace, ct.State)
	}
	return nil
}
