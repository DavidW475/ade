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

// InstallProxyBinary kopiert das laufende ade-Binary (src) nach dir/ade-proxy, falls es dort
// fehlt oder abweicht, und liefert den Pfad. Der Proxy-Container mountet diese Kopie: Sie
// bleibt bestehen, auch wenn src ersetzt oder (bei go run) gelöscht wird.
func InstallProxyBinary(src, dir string) (string, error) {
	if err := checkStatic(src); err != nil {
		return "", err
	}
	data, err := os.ReadFile(src) //nolint:gosec // G304: eigenes Binary
	if err != nil {
		return "", fmt.Errorf("ade-Binary lesen: %w", err)
	}
	dst := filepath.Join(dir, "ade-proxy")
	if old, err := os.ReadFile(dst); err == nil && sha256.Sum256(old) == sha256.Sum256(data) { //nolint:gosec // G304: Datenverzeichnis
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Das Verzeichnis gehört dem Nutzer; der Container läuft mit dessen UID.
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

// checkStatic prüft, dass das Binary ohne dynamischen Linker läuft (CGO_ENABLED=0): Im
// Proxy-Container passt die C-Bibliothek des Hosts nicht unbedingt.
func checkStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("ade-Binary %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return errors.New("das ade-Binary ist dynamisch gelinkt und läuft so nicht im Proxy-Container – mit CGO_ENABLED=0 bauen (make build)")
		}
	}
	return nil
}
