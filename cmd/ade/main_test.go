package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRunVersionAndHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"version"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "ade ") || !strings.Contains(out.String(), "commit ") {
		t.Errorf("version = %q", out.String())
	}

	out.Reset()
	if err := run([]string{"help"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "start") {
		t.Errorf("help = %q, %v", out.String(), err)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"quatsch"}, &out, &errOut)
	if err == nil || exitCode(err) != 2 {
		t.Fatalf("err = %v", err)
	}
	if err := run(nil, &out, &errOut); err == nil || exitCode(err) != 2 {
		t.Fatalf("ohne Befehl: err = %v", err)
	}
}

func TestServeRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte("listen: 0.0.0.0:7000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run([]string{"serve", "--config", path}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "Loopback") {
		t.Fatalf("err = %v", err)
	}
}
