package browser

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestOpenPrefersFirstAvailable(t *testing.T) {
	var ran []string
	look := func(name string) (string, error) {
		if name == "wslview" {
			return "", exec.ErrNotFound
		}
		return "/bin/" + name, nil
	}
	runFn := func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}
	if err := open("http://x", candidates(true), look, runFn); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != "/bin/rundll32.exe url.dll,FileProtocolHandler http://x" {
		t.Fatalf("ran = %v", ran)
	}
}

func TestOpenNoOpener(t *testing.T) {
	look := func(string) (string, error) { return "", exec.ErrNotFound }
	err := open("http://x", candidates(false), look, nil)
	if !errors.Is(err, ErrNoOpener) {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenFallsBackOnError(t *testing.T) {
	var ran []string
	look := func(name string) (string, error) { return name, nil }
	runFn := func(name string, _ ...string) error {
		ran = append(ran, name)
		if name == "wslview" {
			return errors.New("kaputt")
		}
		return nil
	}
	if err := open("http://x", candidates(true), look, runFn); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 2 || ran[1] != "rundll32.exe" {
		t.Fatalf("ran = %v", ran)
	}
}

func TestOpenIgnoresExplorerExitCode(t *testing.T) {
	look := func(name string) (string, error) {
		if name == "explorer.exe" {
			return name, nil
		}
		return "", exec.ErrNotFound
	}
	runFn := func(string, ...string) error { return exec.Command("false").Run() }
	if err := open("http://x", candidates(true), look, runFn); err != nil {
		t.Fatalf("explorer.exe exit code was not ignored: %v", err)
	}
}
