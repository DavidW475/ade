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

const gcUsage = `Usage:
  ade gc [--dry-run]

Removes orphaned sandbox containers: containers of this ADE instance without a workspace and
containers of other instances whose data directory no longer exists. Containers of existing
workspaces stay. If the daemon is running, it does this itself.
`

func cmdGC(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, gcUsage) }
	dry := fs.Bool("dry-run", false, "only list, remove nothing")
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

	verb := "removed"
	if *dry {
		verb = "would remove"
	}
	if len(removed) == 0 {
		_, _ = fmt.Fprintln(stdout, "No orphaned sandbox containers.")
		return nil
	}
	for _, ct := range removed {
		_, _ = fmt.Fprintf(stdout, "%s %s (workspace %s, %s)\n", verb, ct.Name, ct.Workspace, ct.State)
	}
	return nil
}
