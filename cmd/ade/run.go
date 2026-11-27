package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/daemon"
	"github.com/DavidW475/ade/internal/factory"
	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
)

const runUsage = `Verwendung:
  ade run <fabrik> --task "…" [optionen]

Startet einen Run der Fabrik (.ade/factories/<fabrik>.yaml im Projekt oder eine
mitgelieferte Vorlage wie feature) und gibt Run-ID und Dashboard-Link aus.

Optionen:
  --task TEXT        Aufgabe (Eingabe task)
  --input NAME=WERT  weitere Eingabe (mehrfach möglich)
  --project REF      Projekt (ID, Name oder Pfad; Standard: Repo des aktuellen Verzeichnisses)
  --sandbox          Workspace in der Docker-Sandbox
  --no-sandbox       Workspace ohne Sandbox (Standard: defaults.sandbox)
  --wait             bis zum Ende warten und den Fortschritt ausgeben
  --config PFAD      Konfigurationsdatei
`

type inputFlags map[string]string

func (f inputFlags) String() string { return "" }

func (f inputFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return errors.New("Format NAME=WERT")
	}
	f[k] = val
	return nil
}

func cmdRun(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, runUsage) }
	task := fs.String("task", "", "Aufgabe")
	inputs := inputFlags{}
	fs.Var(inputs, "input", "weitere Eingabe NAME=WERT")
	ref := fs.String("project", "", "Projekt")
	withSandbox := fs.Bool("sandbox", false, "Workspace in der Sandbox")
	noSandbox := fs.Bool("no-sandbox", false, "Workspace ohne Sandbox")
	wait := fs.Bool("wait", false, "bis zum Ende warten")
	// Der Fabrik-Name darf vor den Optionen stehen (ade run feature --task …).
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	cfg, cfgPath, err := loadConfig(fs, args, true)
	if err != nil {
		return err
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("unerwartete Argumente: %v", fs.Args())}
	}
	if name == "" {
		_, _ = fmt.Fprint(stderr, runUsage)
		return usageError{"Fabrik fehlt"}
	}
	if *withSandbox && *noSandbox {
		return usageError{"--sandbox und --no-sandbox schließen sich aus"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, _, err := daemon.EnsureRunning(ctx, cfg, cfgPath); err != nil {
		return err
	}
	c := daemon.NewClient(cfg)

	projectRef := *ref
	if projectRef == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		top, err := git.Output(ctx, wd, "rev-parse", "--show-toplevel")
		if err != nil {
			return errors.New("kein Git-Repository im aktuellen Verzeichnis – Projekt mit --project angeben")
		}
		projectRef = top
	}
	if _, err := os.Stat(projectRef); err == nil { //nolint:gosec // G703: Pfad stammt vom Nutzer selbst
		if abs, err := filepath.Abs(projectRef); err == nil {
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				projectRef = resolved
			}
		}
	}
	var p project.View
	if err := c.Do(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(projectRef), nil, &p); err != nil {
		var ae *daemon.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return fmt.Errorf("%s ist kein registriertes Projekt – erst mit „ade project add“ registrieren", projectRef)
		}
		return err
	}

	req := factory.StartRequest{ProjectID: p.ID, Factory: name, Task: *task, Inputs: inputs}
	switch {
	case *withSandbox:
		req.Sandbox = boolPtr(true)
	case *noSandbox:
		req.Sandbox = boolPtr(false)
	}
	var v factory.View
	if err := c.Do(ctx, http.MethodPost, "/api/runs", req, &v); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Run %s der Fabrik %s gestartet (Projekt %s, Workspace %s)\n  %s/runs/%s\n",
		v.ID, v.Factory, p.Name, v.WorkspaceID, cfg.BaseURL(), v.ID)
	if !*wait {
		return nil
	}
	return waitRun(c, v.ID, stdout)
}

func boolPtr(b bool) *bool { return &b }

// waitRun gibt Statuswechsel aus, bis der Run endet. Ein fehlgeschlagener oder
// abgebrochener Run ist ein Fehler.
func waitRun(c *daemon.Client, id string, stdout io.Writer) error {
	last := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var v factory.View
		err := c.Do(ctx, http.MethodGet, "/api/runs/"+id, nil, &v)
		cancel()
		if err != nil {
			return err
		}
		line := v.Status
		if v.Current < len(v.Steps) {
			st := v.Steps[v.Current]
			line = fmt.Sprintf("%s – Schritt %s (%s)", v.Status, st.Name, st.Status)
			if st.Status == store.StepWaiting {
				line += " – wartet auf Freigabe im Dashboard"
			}
		}
		if line != last {
			_, _ = fmt.Fprintf(stdout, "%s  %s\n", time.Now().Format("15:04:05"), line)
			last = line
		}
		switch v.Status {
		case store.RunDone:
			if v.PRURL != "" {
				_, _ = fmt.Fprintln(stdout, "Pull Request:", v.PRURL)
			}
			return nil
		case store.RunFailed, store.RunCanceled:
			return fmt.Errorf("Run %s: %s", v.Status, v.Error)
		}
		time.Sleep(2 * time.Second)
	}
}
