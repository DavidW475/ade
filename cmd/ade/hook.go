package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/DavidW475/ade/internal/approval"
)

const hookUsage = `Usage:
  ade hook pretooluse

PreToolUse hook for Claude Code: reads the hook input from stdin, asks ADE and writes the
decision (allow/deny) to stdout. ADE registers it itself when a Claude session starts
(doc/architecture.md#43-approvals); address and token come from ` + approval.EnvHookURL + `
and ` + approval.EnvHookToken + `. On errors the decision is deny.
`

func cmdHook(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) != 1 || args[0] != "pretooluse" {
		_, _ = fmt.Fprint(stderr, hookUsage)
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
			return nil
		}
		return usageError{"expected: ade hook pretooluse"}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := approval.RunHook(ctx, stdin, stdout, os.Getenv, nil); err != nil {
		// The decision (deny) is already on stdout; exit code 0 so the CLI reads it.
		_, _ = fmt.Fprintln(stderr, "ade hook:", err)
	}
	return nil
}
