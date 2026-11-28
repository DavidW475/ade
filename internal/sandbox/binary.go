package sandbox

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// InstallProxyBinary copies the running ade binary (src) to dir/ade-proxy if it is missing or
// differs there, and returns the path. The proxy container mounts this copy: it stays even
// when src is replaced or (with go run) deleted.
func InstallProxyBinary(src, dir string) (string, error) {
	if err := checkStatic(src); err != nil {
		return "", err
	}
	data, err := os.ReadFile(src) //nolint:gosec // G304: own binary
	if err != nil {
		return "", fmt.Errorf("read ade binary: %w", err)
	}
	dst := filepath.Join(dir, "ade-proxy")
	if old, err := os.ReadFile(dst); err == nil && sha256.Sum256(old) == sha256.Sum256(data) { //nolint:gosec // G304: data directory
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// The directory belongs to the user; the container runs with their UID.
	tmp, err := os.CreateTemp(dir, ".ade-proxy-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o500); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}

// checkStatic checks that the binary runs without a dynamic linker (CGO_ENABLED=0): the
// host's C library does not necessarily fit in the proxy container.
func checkStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("ade binary %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return errors.New("the ade binary is dynamically linked and does not run in the proxy container this way – build with CGO_ENABLED=0 (make build)")
		}
	}
	return nil
}
