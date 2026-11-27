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

const hookUsage = `Verwendung:
  ade hook pretooluse

PreToolUse-Hook für Claude Code: liest die Hook-Eingabe von stdin, fragt ADE und schreibt die
Entscheidung (allow/deny) auf stdout. ADE bindet ihn beim Start einer Claude-Session selbst
ein (doc/architecture.md#43-freigaben); Adresse und Token kommen aus ` + approval.EnvHookURL + `
und ` + approval.EnvHookToken + `. Bei Fehlern lautet die Entscheidung deny.
`

func cmdHook(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) != 1 || args[0] != "pretooluse" {
		_, _ = fmt.Fprint(stderr, hookUsage)
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
			return nil
		}
		return usageError{"erwartet: ade hook pretooluse"}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := approval.RunHook(ctx, stdin, stdout, os.Getenv, nil); err != nil {
		// Die Entscheidung (deny) steht schon auf stdout; Exit-Code 0, damit die CLI sie liest.
		_, _ = fmt.Fprintln(stderr, "ade hook:", err)
	}
	return nil
}
