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

// Version is the openvscode-server version that ADE downloads and has tested.
// When raising it, update the checksums as well.
const Version = "1.109.5"

// checksums are the SHA-256 sums of the release archives per GOARCH. The releases on GitHub
// do not publish their own checksums; the values were computed from the archives when the
// version was pinned.
var checksums = map[string]string{
	"amd64": "b433bf4f0227321a7014d8460d10a8f958adc0f45aa79bd889e84e65e8f88363",
	"arm64": "36d9c14036489b63de84ebace837fcacf7e60e669a0dc715802c5443684ea4dc",
}

// releaseArch translates GOARCH into the names of the release archives.
var releaseArch = map[string]string{"amd64": "x64", "arm64": "arm64"}

// Limits for the archive (the release archives are about 75 MB, about 250 MB unpacked).
const (
	maxDownload  = 512 << 20
	maxExtracted = 4 << 30
)

// release describes a downloadable archive.
type release struct {
	Name   string // directory name in the archive, e.g. openvscode-server-v1.109.5-linux-x64
	URL    string
	SHA256 string
}

func defaultRelease() (release, error) {
	arch, ok := releaseArch[runtime.GOARCH]
	sum := checksums[runtime.GOARCH]
	if !ok || sum == "" {
		return release{}, fmt.Errorf("there is no openvscode-server for the architecture %s; set ide.binary in the configuration", runtime.GOARCH)
	}
	name := fmt.Sprintf("openvscode-server-v%s-linux-%s", Version, arch)
	return release{
		Name:   name,
		URL:    fmt.Sprintf("https://github.com/gitpod-io/openvscode-server/releases/download/openvscode-server-v%s/%s.tar.gz", Version, name),
		SHA256: sum,
	}, nil
}

// binaryIn is the path of the start script in an unpacked release.
func binaryIn(dir string) string { return filepath.Join(dir, "bin", "openvscode-server") }

// install downloads rel, verifies the checksum and unpacks it to dir/rel.Name.
// If the release is already there, nothing happens. It is unpacked into a temporary
// directory that is only renamed after success – an aborted download leaves no half
// installation behind.
func install(ctx context.Context, client *http.Client, dir string, rel release) (string, error) {
	target := filepath.Join(dir, rel.Name)
	if fileExists(binaryIn(target)) {
		return binaryIn(target), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create IDE directory: %w", err)
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
		return "", fmt.Errorf("unpack openvscode-server: %w", err)
	}
	src := filepath.Join(tmp, rel.Name)
	if !fileExists(binaryIn(src)) {
		return "", fmt.Errorf("archive contains no %s/bin/openvscode-server", rel.Name)
	}
	if err := os.Rename(src, target); err != nil {
		if fileExists(binaryIn(target)) { // installed in parallel
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
		return fmt.Errorf("download openvscode-server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download openvscode-server: %s returns %s", url, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return fmt.Errorf("download openvscode-server: %w", err)
	}
	if n > maxDownload {
		return errors.New("download openvscode-server: archive is too large")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
		return fmt.Errorf("openvscode-server: checksum does not match (expected %s, got %s)", wantSum, got)
	}
	return nil
}

// extract unpacks a tar.gz to dir. Allowed are directories, files and relative symlinks
// without “..”; paths outside dir are an error.
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
			return fmt.Errorf("invalid path %q in the archive", hdr.Name)
		}
		path := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += hdr.Size; hdr.Size < 0 || total > maxExtracted {
				return errors.New("archive is too large when unpacked")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			if err := writeFile(path, tr, hdr.Size, os.FileMode(hdr.Mode)&0o700|0o600); err != nil { //nolint:gosec // G115: the mode is masked
				return err
			}
		case tar.TypeSymlink:
			// Only targets below the link's directory: without “..” even a chain of symlinks
			// cannot lead out of dir.
			target := hdr.Linkname
			if target == "" || filepath.IsAbs(target) || slices.Contains(strings.Split(filepath.ToSlash(target), "/"), "..") {
				return fmt.Errorf("symlink %q → %q is not allowed", hdr.Name, target)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			if err := os.Symlink(target, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q (type %c) in the archive", hdr.Name, hdr.Typeflag)
		}
	}
}

func writeFile(path string, r io.Reader, size int64, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // G304: the path is checked
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
