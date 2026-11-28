package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/DavidW475/ade/internal/daemon"
	"github.com/DavidW475/ade/internal/project"
)

const projectUsage = `Usage:
  ade project add <path>             register a Git repository
  ade project list                   list projects
  ade project remove [--force] <id>  unregister a project (ID, name or path);
                                     --force also deletes its workspaces

The repository itself is never changed. If no daemon is running, it is started.
`

func cmdProject(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, projectUsage)
		return usageError{"no subcommand given"}
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("project "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, projectUsage) }
	force := false
	if sub == "remove" {
		fs.BoolVar(&force, "force", false, "also delete all workspaces of the project")
	}
	cfg, cfgPath, err := loadConfig(fs, rest, true)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, _, err := daemon.EnsureRunning(ctx, cfg, cfgPath); err != nil {
		return err
	}
	c := daemon.NewClient(cfg)

	switch sub {
	case "add":
		if fs.NArg() != 1 {
			return usageError{"ade project add <path>"}
		}
		abs, err := filepath.Abs(fs.Arg(0))
		if err != nil {
			return err
		}
		var p project.View
		if err := c.Do(ctx, http.MethodPost, "/api/projects", map[string]string{"path": abs}, &p); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "project %q registered (ID %s, base branch %s)\n  %s\n", p.Name, p.ID, p.BaseBranch, p.RepoPath)
		printProjectNotes(stderr, &p)
		return nil

	case "list":
		if fs.NArg() != 0 {
			return usageError{"ade project list"}
		}
		var ps []project.View
		if err := c.Do(ctx, http.MethodGet, "/api/projects", nil, &ps); err != nil {
			return err
		}
		if len(ps) == 0 {
			_, _ = fmt.Fprintln(stdout, "No projects. Register one with: ade project add <path>")
			return nil
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tBASIS\tPFAD\tHINWEIS")
		for _, p := range ps {
			note := p.ConfigError
			if note == "" && len(p.Warnings) > 0 {
				note = "is under /mnt"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, p.BaseBranch, p.RepoPath, note)
		}
		return tw.Flush()

	case "remove":
		if fs.NArg() != 1 {
			return usageError{"ade project remove [--force] <id|name|path>"}
		}
		ref := fs.Arg(0)
		// An existing path is resolved like for add.
		if _, err := os.Stat(ref); err == nil { //nolint:gosec // G703: the path comes from the user themselves

			if abs, err := filepath.Abs(ref); err == nil {
				if resolved, err := filepath.EvalSymlinks(abs); err == nil {
					ref = resolved
				}
			}
		}
		var p project.View
		if err := c.Do(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(ref), nil, &p); err != nil {
			return err
		}
		q := ""
		if force {
			q = "?force=true"
		}
		if err := c.Do(ctx, http.MethodDelete, "/api/projects/"+p.ID+q, nil, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "project %q (%s) unregistered\n", p.Name, p.ID)
		return nil

	default:
		_, _ = fmt.Fprint(stderr, projectUsage)
		return usageError{fmt.Sprintf("unknown subcommand %q", sub)}
	}
}

func printProjectNotes(w io.Writer, p *project.View) {
	for _, warn := range p.Warnings {
		_, _ = fmt.Fprintln(w, "warning:", warn)
	}
	if p.ConfigError != "" {
		_, _ = fmt.Fprintln(w, "error in the project configuration:", p.ConfigError)
	}
}
