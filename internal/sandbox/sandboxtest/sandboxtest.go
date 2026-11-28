// Package sandboxtest provides image and ade binary for Docker integration tests.
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

// ImageTag is the small test image (git, curl, getent, busybox).
const ImageTag = "ade-test-git:2"

// Fully qualified, because Podman rejects short names without a registry configuration.
const dockerfile = `FROM docker.io/library/debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl busybox && rm -rf /var/lib/apt/lists/*
`

// Docker returns the tests' Docker CLI: ADE_TEST_DOCKER_BIN (e.g. podman) or docker.
func Docker() string {
	if b := os.Getenv("ADE_TEST_DOCKER_BIN"); b != "" {
		return b
	}
	return "docker"
}

// Enabled skips the test without ADE_TEST_DOCKER=1.
func Enabled(t testing.TB) {
	t.Helper()
	if os.Getenv("ADE_TEST_DOCKER") != "1" {
		t.Skip("Docker tests only with ADE_TEST_DOCKER=1")
	}
}

// Image returns ADE_TEST_IMAGE or builds the test image.
func Image(t testing.TB) string {
	t.Helper()
	Enabled(t)
	if img := os.Getenv("ADE_TEST_IMAGE"); img != "" {
		return img
	}
	if exec.Command(Docker(), "image", "inspect", ImageTag).Run() == nil {
		return ImageTag
	}
	cmd := exec.Command(Docker(), "build", "-t", ImageTag, "-f", "-", t.TempDir()) //nolint:gosec // G204: test
	cmd.Stdin = strings.NewReader(dockerfile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test image: %v\n%s", err, out)
	}
	return ImageTag
}

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// ProxyBinary builds the ade binary once per test process (statically, like make build).
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
			binErr = fmt.Errorf("build ade: %w\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}
