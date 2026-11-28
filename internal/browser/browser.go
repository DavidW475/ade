// Package browser opens URLs in the browser – under WSL in the Windows browser.
package browser

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// ErrNoOpener means that no program to open a URL was found.
var ErrNoOpener = errors.New("no program to open the browser found")

type opener struct {
	name string
	args []string
	// ignoreExit: explorer.exe returns exit code 1 even on success.
	ignoreExit bool
}

// IsWSL reports whether the program runs in a WSL distribution.
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
			// rundll32 passes the URL including the query string to the default browser;
			// explorer.exe loses it depending on the Windows version.
			{name: "rundll32.exe", args: []string{"url.dll,FileProtocolHandler"}},
			{name: "explorer.exe", ignoreExit: true},
			{name: "xdg-open"},
		}
	}
	return []opener{{name: "xdg-open"}}
}

// Open opens url in the default browser. Under WSL the Windows browser is used
// (wslview, otherwise rundll32.exe or explorer.exe).
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
