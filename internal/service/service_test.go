package service

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type fakeSystemctl struct {
	calls   [][]string
	noSysd  bool
	failCmd string
	active  bool
}

func (f *fakeSystemctl) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.noSysd {
		return []byte("Failed to connect to bus: No medium found\n"), errors.New("exit status 1")
	}
	if args[0] == f.failCmd {
		return []byte("kaputt"), errors.New("exit status 1")
	}
	if args[0] == "is-active" {
		if f.active {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), errors.New("exit status 3")
	}
	return nil, nil
}

func newTest(t *testing.T, f *fakeSystemctl) *Manager {
	t.Helper()
	return &Manager{
		UnitDir:    t.TempDir() + "/systemd/user",
		Exe:        "/home/u/.local/bin/ade",
		ConfigPath: "/home/u/.config/ade/config.yaml",
		LogPath:    "/home/u/.local/share/ade/daemon.log",
		Path:       "/home/u/.local/bin:/usr/bin",
		Run:        f.run,
	}
}

func TestUnit(t *testing.T) {
	m := newTest(t, &fakeSystemctl{})
	u := m.Unit()
	for _, want := range []string{
		`ExecStart="/home/u/.local/bin/ade" serve --config "/home/u/.config/ade/config.yaml"`,
		`Environment="PATH=/home/u/.local/bin:/usr/bin"`,
		"StandardError=append:/home/u/.local/share/ade/daemon.log",
		"Restart=on-failure",
		"WantedBy=default.target",
		configMarker + "/home/u/.config/ade/config.yaml",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("Unit enthält nicht %q:\n%s", want, u)
		}
	}
}

func TestUnitEscaping(t *testing.T) {
	m := newTest(t, &fakeSystemctl{})
	m.Exe = `/opt/my "ade"/100%/$HOME\x`
	u := m.Unit()
	want := `ExecStart="/opt/my \"ade\"/100%%/$$HOME\\x" serve`
	if !strings.Contains(u, want) {
		t.Errorf("Unit enthält nicht %q:\n%s", want, u)
	}
}

func TestInstall(t *testing.T) {
	f := &fakeSystemctl{}
	m := newTest(t, f)
	if m.Installed() {
		t.Fatal("vor Install installiert")
	}
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"show-environment"}, {"daemon-reload"}, {"enable", Name}, {"restart", Name}}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("Aufrufe = %v, erwartet %v", f.calls, want)
	}
	data, err := os.ReadFile(m.UnitPath())
	if err != nil || string(data) != m.Unit() {
		t.Fatalf("Unit-Datei = %q, %v", data, err)
	}
	if !m.Installed() || m.InstalledConfig() != m.ConfigPath {
		t.Errorf("Installed = %v, Config = %q", m.Installed(), m.InstalledConfig())
	}
}

func TestInstallWithoutSystemd(t *testing.T) {
	f := &fakeSystemctl{noSysd: true}
	m := newTest(t, f)
	err := m.Install(context.Background())
	if !errors.Is(err, ErrNoSystemd) || !strings.Contains(err.Error(), "systemd=true") {
		t.Fatalf("err = %v", err)
	}
	if m.Installed() {
		t.Error("Unit trotz Fehler geschrieben")
	}
}

func TestInstallSystemctlFails(t *testing.T) {
	f := &fakeSystemctl{failCmd: "enable"}
	m := newTest(t, f)
	err := m.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "enable") || !strings.Contains(err.Error(), "kaputt") {
		t.Fatalf("err = %v", err)
	}
}

func TestUninstall(t *testing.T) {
	f := &fakeSystemctl{}
	m := newTest(t, f)
	if err := m.Uninstall(context.Background()); err == nil {
		t.Fatal("Uninstall ohne Unit ohne Fehler")
	}
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Installed() {
		t.Error("Unit noch vorhanden")
	}
	want := [][]string{{"show-environment"}, {"disable", "--now", Name}, {"show-environment"}, {"daemon-reload"}}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("Aufrufe = %v, erwartet %v", f.calls, want)
	}
}

func TestUninstallWithoutSystemdRemovesUnit(t *testing.T) {
	f := &fakeSystemctl{}
	m := newTest(t, f)
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.noSysd = true
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Installed() {
		t.Error("Unit noch vorhanden")
	}
}

func TestActive(t *testing.T) {
	f := &fakeSystemctl{}
	m := newTest(t, f)
	if m.Active(context.Background()) {
		t.Error("inaktiv als aktiv gemeldet")
	}
	f.active = true
	if !m.Active(context.Background()) {
		t.Error("aktiv als inaktiv gemeldet")
	}
}
