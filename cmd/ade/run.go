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

const runUsage = `Usage:
  ade run <factory> --task "…" [options]

Starts a run of the factory (.ade/factories/<factory>.yaml in the project or a built-in
template like feature) and prints the run ID and dashboard link.

Options:
  --task TEXT        task (input task)
  --input NAME=VALUE further input (can be repeated)
  --project REF      project (ID, name or path; default: repository of the current directory)
  --sandbox          workspace in the Docker sandbox
  --no-sandbox       workspace without sandbox (default: defaults.sandbox)
  --wait             wait until the end and print the progress
  --config PATH      configuration file
`

type inputFlags map[string]string

func (f inputFlags) String() string { return "" }

func (f inputFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return errors.New("format NAME=VALUE")
	}
	f[k] = val
	return nil
}

func cmdRun(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, runUsage) }
	task := fs.String("task", "", "task")
	inputs := inputFlags{}
	fs.Var(inputs, "input", "further input NAME=VALUE")
	ref := fs.String("project", "", "project")
	withSandbox := fs.Bool("sandbox", false, "workspace in the sandbox")
	noSandbox := fs.Bool("no-sandbox", false, "workspace without sandbox")
	wait := fs.Bool("wait", false, "wait until the end")
	// The factory name may come before the options (ade run feature --task …).
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
		return usageError{"factory missing"}
	}
	if *withSandbox && *noSandbox {
		return usageError{"--sandbox and --no-sandbox exclude each other"}
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
			return errors.New("no Git repository in the current directory – specify the project with --project")
		}
		projectRef = top
	}
	if _, err := os.Stat(projectRef); err == nil { //nolint:gosec // G703: the path comes from the user themselves
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
			return fmt.Errorf("%s is not a registered project – register it first with “ade project add”", projectRef)
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
	_, _ = fmt.Fprintf(stdout, "run %s of factory %s started (project %s, workspace %s)\n  %s/runs/%s\n",
		v.ID, v.Factory, p.Name, v.WorkspaceID, cfg.BaseURL(), v.ID)
	if !*wait {
		return nil
	}
	return waitRun(c, v.ID, stdout)
}

func boolPtr(b bool) *bool { return &b }

// waitRun prints status changes until the run ends. A failed or canceled run is an
// error.
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
			line = fmt.Sprintf("%s – step %s (%s)", v.Status, st.Name, st.Status)
			if st.Status == store.StepWaiting {
				line += " – waiting for approval in the dashboard"
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
			return fmt.Errorf("run %s: %s", v.Status, v.Error)
		}
		time.Sleep(2 * time.Second)
	}
}
