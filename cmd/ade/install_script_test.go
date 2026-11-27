package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease legt ein Release-Verzeichnis mit Archiv und checksums.txt an.
func fakeRelease(t *testing.T, badSum bool) string {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "tar", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s fehlt", tool)
		}
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		t.Skipf("Architektur %s", arch)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	bin := []byte("#!/bin/sh\necho \"ade v9.9.9 (fake)\"\n")
	for _, f := range []struct {
		name string
		data []byte
		mode int64
	}{{"LICENSE", []byte("Apache"), 0o644}, {"ade", bin, 0o755}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "ade_linux_" + arch + ".tar.gz"
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	hexSum := hex.EncodeToString(sum[:])
	if badSum {
		hexSum = strings.Repeat("0", 64)
	}
	sums := fmt.Sprintf("%s  ade_linux_other.tar.gz\n%s  %s\n", strings.Repeat("a", 64), hexSum, name)
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runInstall(t *testing.T, release, target string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"../../install.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "ADE_BASE_URL=file://"+release, "ADE_INSTALL_DIR="+target, "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallScript(t *testing.T) {
	release := fakeRelease(t, false)
	target := filepath.Join(t.TempDir(), "bin")
	out, err := runInstall(t, release, target)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "ade v9.9.9 (fake)") || !strings.Contains(out, "nicht im PATH") {
		t.Errorf("Ausgabe:\n%s", out)
	}
	fi, err := os.Stat(filepath.Join(target, "ade"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("Binary: %v, %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(target, "LICENSE")); err == nil {
		t.Error("LICENSE mitinstalliert")
	}
	// Ein zweiter Lauf ersetzt das Binary.
	if out, err := runInstall(t, release, target); err != nil {
		t.Fatalf("zweiter Lauf: %v\n%s", err, out)
	}
}

func TestInstallScriptBadChecksum(t *testing.T) {
	release := fakeRelease(t, true)
	target := filepath.Join(t.TempDir(), "bin")
	out, err := runInstall(t, release, target)
	if err == nil || !strings.Contains(out, "Prüfsumme stimmt nicht") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(target, "ade")); err == nil {
		t.Error("Binary trotz falscher Prüfsumme installiert")
	}
}

func TestInstallScriptUnknownOption(t *testing.T) {
	release := fakeRelease(t, false)
	out, err := runInstall(t, release, t.TempDir(), "--quatsch")
	if err == nil || !strings.Contains(out, "unbekannte Option") {
		t.Fatalf("err = %v\n%s", err, out)
	}
}
