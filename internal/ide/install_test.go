package ide

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func makeTarGz(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func goodArchive(t *testing.T, name string) []byte {
	return makeTarGz(t, []entry{
		{name: name + "/", typ: tar.TypeDir},
		{name: name + "/bin/", typ: tar.TypeDir},
		{name: name + "/bin/openvscode-server", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o755},
		{name: name + "/product.json", typ: tar.TypeReg, body: `{"extensionsGallery":{"serviceUrl":"https://open-vsx.org/vscode/gallery"}}`},
		{name: name + "/lib", typ: tar.TypeSymlink, link: "bin"},
	})
}

func serve(t *testing.T, data []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestInstall(t *testing.T) {
	const name = "openvscode-server-vtest-linux-x64"
	data := goodArchive(t, name)
	srv, hits := serve(t, data)
	dir := t.TempDir()
	rel := release{Name: name, URL: srv.URL, SHA256: sum(data)}

	bin, err := install(context.Background(), srv.Client(), dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	if bin != filepath.Join(dir, name, "bin", "openvscode-server") {
		t.Errorf("bin = %s", bin)
	}
	st, err := os.Stat(bin)
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("Binary nicht ausführbar: %v %v", st, err)
	}
	if l, err := os.Readlink(filepath.Join(dir, name, "lib")); err != nil || l != "bin" {
		t.Errorf("Symlink = %q, %v", l, err)
	}
	// Keine Reste von Download oder Entpacken.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("Verzeichnis enthält %d Einträge, want 1", len(ents))
	}
	// Zweiter Aufruf lädt nicht neu.
	if _, err := install(context.Background(), srv.Client(), dir, rel); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("%d Downloads, want 1", hits.Load())
	}
}

func TestInstallRejectsWrongChecksum(t *testing.T) {
	const name = "ovs"
	data := goodArchive(t, name)
	srv, _ := serve(t, data)
	dir := t.TempDir()
	_, err := install(context.Background(), srv.Client(), dir, release{Name: name, URL: srv.URL, SHA256: strings.Repeat("0", 64)})
	if err == nil || !strings.Contains(err.Error(), "Prüfsumme") {
		t.Fatalf("err = %v, want Prüfsummenfehler", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("Reste nach Fehler: %v", ents)
	}
}

func TestExtractRejectsUnsafeEntries(t *testing.T) {
	cases := map[string][]entry{
		"pfad-nach-oben":   {{name: "../evil", typ: tar.TypeReg, body: "x"}},
		"absoluter-pfad":   {{name: "/tmp/evil", typ: tar.TypeReg, body: "x"}},
		"symlink-heraus":   {{name: "a/link", typ: tar.TypeSymlink, link: "../../etc"}},
		"absoluter-link":   {{name: "link", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"hardlink":         {{name: "hl", typ: tar.TypeLink, link: "x"}},
		"geraetedatei":     {{name: "dev", typ: tar.TypeChar}},
		"datei-durch-link": {{name: "l", typ: tar.TypeSymlink, link: "."}, {name: "l", typ: tar.TypeReg, body: "x"}},
		"link-kette": {
			{name: "sub", typ: tar.TypeSymlink, link: "."},
			{name: "s1", typ: tar.TypeSymlink, link: "sub/sub"},
			{name: "s2", typ: tar.TypeSymlink, link: "s1/.."},
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := extract(bytes.NewReader(makeTarGz(t, entries)), dir); err == nil {
				t.Fatal("Fehler erwartet")
			}
		})
	}
}

func TestManagerDownloadsOnFirstStart(t *testing.T) {
	const name = "ovs-fake"
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Das „Release“ enthält ein Startskript, das die Test-Binary als Fake-Server aufruft.
	script := "#!/bin/sh\nexec " + exe + " \"$@\"\n"
	data := makeTarGz(t, []entry{{name: name + "/bin/openvscode-server", typ: tar.TypeReg, body: script, mode: 0o755}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond) // Zustand „installing“ sichtbar machen
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t, "ok", func(o *Options) {
		o.Binary = ""
		o.HTTPClient = srv.Client()
		o.release = &release{Name: name, URL: srv.URL, SHA256: sum(data)}
	})
	if _, err := f.m.Start(context.Background(), "ws1"); err != nil {
		t.Fatal(err)
	}
	waitState(t, f.m, "ws1", StateInstalling)
	waitState(t, f.m, "ws1", StateRunning)
	if _, err := os.Stat(filepath.Join(f.m.opts.Dir, name, "bin", "openvscode-server")); err != nil {
		t.Error(err)
	}
}
