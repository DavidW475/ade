// Package browser öffnet URLs im Browser – unter WSL im Windows-Browser.
package browser

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// ErrNoOpener bedeutet, dass kein Programm zum Öffnen einer URL gefunden wurde.
var ErrNoOpener = errors.New("kein Programm zum Öffnen des Browsers gefunden")

type opener struct {
	name string
	args []string
	// ignoreExit: explorer.exe liefert auch bei Erfolg Exit-Code 1.
	ignoreExit bool
}

// IsWSL meldet, ob das Programm in einer WSL-Distribution läuft.
func IsWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
}

func candidates(wsl bool) []opener {
	if wsl {
		return []opener{
			{name: "wslview"},
			// rundll32 übergibt die URL samt Query-String an den Standardbrowser;
			// explorer.exe verliert sie je nach Windows-Version.
			{name: "rundll32.exe", args: []string{"url.dll,FileProtocolHandler"}},
			{name: "explorer.exe", ignoreExit: true},
			{name: "xdg-open"},
		}
	}
	return []opener{{name: "xdg-open"}}
}

// Open öffnet url im Standardbrowser. Unter WSL wird der Windows-Browser verwendet
// (wslview, sonst rundll32.exe bzw. explorer.exe).
func Open(url string) error {
	return open(url, candidates(IsWSL()), exec.LookPath, run)
}

func open(url string, cs []opener, lookPath func(string) (string, error), run func(string, ...string) error) error {
	lastErr := ErrNoOpener
	for _, c := range cs {
		path, err := lookPath(c.name)
		if err != nil {
			continue
		}
		err = run(path, append(c.args, url)...)
		var exitErr *exec.ExitError
		if err == nil || (c.ignoreExit && errors.As(err, &exitErr)) {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}
