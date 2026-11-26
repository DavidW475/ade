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

const projectUsage = `Verwendung:
  ade project add <pfad>             Git-Repository registrieren
  ade project list                   Projekte auflisten
  ade project remove [--force] <id>  Projekt abmelden (ID, Name oder Pfad);
                                     --force löscht auch dessen Workspaces

Das Repository selbst wird nie verändert. Läuft kein Daemon, wird er gestartet.
`

func cmdProject(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, projectUsage)
		return usageError{"kein Unterbefehl angegeben"}
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("project "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, projectUsage) }
	force := false
	if sub == "remove" {
		fs.BoolVar(&force, "force", false, "auch alle Workspaces des Projekts löschen")
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
			return usageError{"ade project add <pfad>"}
		}
		abs, err := filepath.Abs(fs.Arg(0))
		if err != nil {
			return err
		}
		var p project.View
		if err := c.Do(ctx, http.MethodPost, "/api/projects", map[string]string{"path": abs}, &p); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "Projekt %q registriert (ID %s, Basis-Branch %s)\n  %s\n", p.Name, p.ID, p.BaseBranch, p.RepoPath)
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
			_, _ = fmt.Fprintln(stdout, "Keine Projekte. Registrieren mit: ade project add <pfad>")
			return nil
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tBASIS\tPFAD\tHINWEIS")
		for _, p := range ps {
			note := p.ConfigError
			if note == "" && len(p.Warnings) > 0 {
				note = "liegt unter /mnt"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, p.BaseBranch, p.RepoPath, note)
		}
		return tw.Flush()

	case "remove":
		if fs.NArg() != 1 {
			return usageError{"ade project remove [--force] <id|name|pfad>"}
		}
		ref := fs.Arg(0)
		// Ein existierender Pfad wird wie bei add aufgelöst.
		if _, err := os.Stat(ref); err == nil { //nolint:gosec // G703: Pfad stammt vom Nutzer selbst

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
		_, _ = fmt.Fprintf(stdout, "Projekt %q (%s) abgemeldet\n", p.Name, p.ID)
		return nil

	default:
		_, _ = fmt.Fprint(stderr, projectUsage)
		return usageError{fmt.Sprintf("unbekannter Unterbefehl %q", sub)}
	}
}

func printProjectNotes(w io.Writer, p *project.View) {
	for _, warn := range p.Warnings {
		_, _ = fmt.Fprintln(w, "Warnung:", warn)
	}
	if p.ConfigError != "" {
		_, _ = fmt.Fprintln(w, "Fehler in der Projekt-Konfiguration:", p.ConfigError)
	}
}
