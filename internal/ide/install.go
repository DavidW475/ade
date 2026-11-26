package ide

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Version ist die openvscode-server-Version, die ADE herunterlädt und getestet hat.
// Beim Anheben die Prüfsummen in checksums mit aktualisieren.
const Version = "1.109.5"

// checksums sind die SHA-256-Summen der Release-Archive je GOARCH. Die Releases auf GitHub
// veröffentlichen keine eigenen Prüfsummen; die Werte wurden beim Festlegen der Version
// aus den Archiven berechnet.
var checksums = map[string]string{
	"amd64": "b433bf4f0227321a7014d8460d10a8f958adc0f45aa79bd889e84e65e8f88363",
	"arm64": "36d9c14036489b63de84ebace837fcacf7e60e669a0dc715802c5443684ea4dc",
}

// releaseArch übersetzt GOARCH in die Namen der Release-Archive.
var releaseArch = map[string]string{"amd64": "x64", "arm64": "arm64"}

// Grenzen für das Archiv (die Release-Archive sind etwa 75 MB groß, entpackt etwa 250 MB).
const (
	maxDownload  = 512 << 20
	maxExtracted = 4 << 30
)

// release beschreibt ein herunterladbares Archiv.
type release struct {
	Name   string // Verzeichnisname im Archiv, z. B. openvscode-server-v1.109.5-linux-x64
	URL    string
	SHA256 string
}

func defaultRelease() (release, error) {
	arch, ok := releaseArch[runtime.GOARCH]
	sum := checksums[runtime.GOARCH]
	if !ok || sum == "" {
		return release{}, fmt.Errorf("für die Architektur %s gibt es keinen openvscode-server; ide.binary in der Konfiguration setzen", runtime.GOARCH)
	}
	name := fmt.Sprintf("openvscode-server-v%s-linux-%s", Version, arch)
	return release{
		Name:   name,
		URL:    fmt.Sprintf("https://github.com/gitpod-io/openvscode-server/releases/download/openvscode-server-v%s/%s.tar.gz", Version, name),
		SHA256: sum,
	}, nil
}

// binaryIn ist der Pfad des Startskripts in einem entpackten Release.
func binaryIn(dir string) string { return filepath.Join(dir, "bin", "openvscode-server") }

// install lädt rel herunter, prüft die Prüfsumme und entpackt es nach dir/rel.Name.
// Liegt das Release schon dort, passiert nichts. Entpackt wird in ein temporäres
// Verzeichnis, das erst nach Erfolg umbenannt wird – ein abgebrochener Download
// hinterlässt keine halbe Installation.
func install(ctx context.Context, client *http.Client, dir string, rel release) (string, error) {
	target := filepath.Join(dir, rel.Name)
	if fileExists(binaryIn(target)) {
		return binaryIn(target), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("IDE-Verzeichnis anlegen: %w", err)
	}

	archive, err := os.CreateTemp(dir, ".download-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	if err := download(ctx, client, rel.URL, archive, rel.SHA256); err != nil {
		return "", err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	tmp, err := os.MkdirTemp(dir, ".install-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := extract(archive, tmp); err != nil {
		return "", fmt.Errorf("openvscode-server entpacken: %w", err)
	}
	src := filepath.Join(tmp, rel.Name)
	if !fileExists(binaryIn(src)) {
		return "", fmt.Errorf("Archiv enthält kein %s/bin/openvscode-server", rel.Name)
	}
	if err := os.Rename(src, target); err != nil {
		if fileExists(binaryIn(target)) { // parallel installiert
			return binaryIn(target), nil
		}
		return "", err
	}
	return binaryIn(target), nil
}

func download(ctx context.Context, client *http.Client, url string, w io.Writer, wantSum string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("openvscode-server herunterladen: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openvscode-server herunterladen: %s liefert %s", url, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return fmt.Errorf("openvscode-server herunterladen: %w", err)
	}
	if n > maxDownload {
		return errors.New("openvscode-server herunterladen: Archiv ist zu groß")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
		return fmt.Errorf("openvscode-server: Prüfsumme stimmt nicht (erwartet %s, erhalten %s)", wantSum, got)
	}
	return nil
}

// extract entpackt ein tar.gz nach dir. Erlaubt sind Verzeichnisse, Dateien und relative
// Symlinks ohne „..“; Pfade außerhalb von dir sind ein Fehler.
func extract(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unzulässiger Pfad %q im Archiv", hdr.Name)
		}
		path := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += hdr.Size; hdr.Size < 0 || total > maxExtracted {
				return errors.New("Archiv ist entpackt zu groß")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			if err := writeFile(path, tr, hdr.Size, os.FileMode(hdr.Mode)&0o700|0o600); err != nil { //nolint:gosec // G115: Mode wird maskiert
				return err
			}
		case tar.TypeSymlink:
			// Nur Ziele unterhalb des Link-Verzeichnisses: ohne „..“ kann auch eine Kette von
			// Symlinks nicht aus dir herausführen.
			target := hdr.Linkname
			if target == "" || filepath.IsAbs(target) || slices.Contains(strings.Split(filepath.ToSlash(target), "/"), "..") {
				return fmt.Errorf("Symlink %q → %q ist nicht erlaubt", hdr.Name, target)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			if err := os.Symlink(target, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("nicht unterstützter Eintrag %q (Typ %c) im Archiv", hdr.Name, hdr.Typeflag)
		}
	}
}

func writeFile(path string, r io.Reader, size int64, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // G304: Pfad ist geprüft
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
