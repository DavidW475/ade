// Package sandboxtest stellt Image und ade-Binary für Docker-Integrationstests bereit.
package sandboxtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ImageTag ist das kleine Test-Image (git, curl, getent, busybox).
const ImageTag = "ade-test-git:2"

// Voll qualifiziert, weil Podman kurze Namen ohne Registry-Konfiguration ablehnt.
const dockerfile = `FROM docker.io/library/debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl busybox && rm -rf /var/lib/apt/lists/*
`

// Docker liefert die Docker-CLI der Tests: ADE_TEST_DOCKER_BIN (z. B. podman) oder docker.
func Docker() string {
	if b := os.Getenv("ADE_TEST_DOCKER_BIN"); b != "" {
		return b
	}
	return "docker"
}

// Enabled überspringt den Test ohne ADE_TEST_DOCKER=1.
func Enabled(t testing.TB) {
	t.Helper()
	if os.Getenv("ADE_TEST_DOCKER") != "1" {
		t.Skip("Docker-Tests nur mit ADE_TEST_DOCKER=1")
	}
}

// Image liefert ADE_TEST_IMAGE oder baut das Test-Image.
func Image(t testing.TB) string {
	t.Helper()
	Enabled(t)
	if img := os.Getenv("ADE_TEST_IMAGE"); img != "" {
		return img
	}
	if exec.Command(Docker(), "image", "inspect", ImageTag).Run() == nil {
		return ImageTag
	}
	cmd := exec.Command(Docker(), "build", "-t", ImageTag, "-f", "-", t.TempDir()) //nolint:gosec // G204: Test
	cmd.Stdin = strings.NewReader(dockerfile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Test-Image bauen: %v\n%s", err, out)
	}
	return ImageTag
}

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// ProxyBinary baut das ade-Binary einmal pro Testprozess (statisch, wie make build).
func ProxyBinary(t testing.TB) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ade-proxy-bin-")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "ade")
		cmd := exec.Command("go", "build", "-o", binPath, "github.com/DavidW475/ade/cmd/ade")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			binErr = fmt.Errorf("ade bauen: %w\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}
