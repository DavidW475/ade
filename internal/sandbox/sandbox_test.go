package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig([]byte("image: ghcr.io/x/y:1\nresources:\n  memory: 2g\n  cpus: 1.5\n  pids: 256\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Image != "ghcr.io/x/y:1" || c.Resources != (Resources{Memory: "2g", CPUs: 1.5, PIDs: 256}) {
		t.Fatalf("Config = %+v", c)
	}
	c, err = ParseConfig(nil)
	if err != nil || c.Resources != (Resources{Memory: DefaultMemory, CPUs: DefaultCPUs, PIDs: DefaultPIDs}) {
		t.Fatalf("leer: %+v, %v", c, err)
	}
	for _, bad := range []string{
		"egress:\n  presets: [npm]\n",
		"image: \"-v /:/host\"\n",
		"resources:\n  memory: viel\n",
		"resources:\n  cpus: -1\n",
		"resources:\n  pids: 3\n",
		"unbekannt: 1\n",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%q: kein Fehler", bad)
		}
	}
}

func TestDefaultImage(t *testing.T) {
	if got := DefaultImage("v1.2.3"); got != ImageRepo+":1.2.3" {
		t.Errorf("Release: %s", got)
	}
	for _, v := range []string{"dev", "v1.2.3-4-gabc-dirty", ""} {
		if got := DefaultImage(v); got != ImageRepo+":latest" {
			t.Errorf("%q: %s", v, got)
		}
	}
}

func TestCreateArgs(t *testing.T) {
	m := New(Options{DataDir: "/data", Image: "img:1", UID: 1234, GID: 5678})
	args := m.CreateArgs(Spec{Workspace: "abc", Path: "/ws/abc", Config: *withDefaults(&Config{}), Env: []string{"GIT_AUTHOR_NAME=X"}})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--name ade-ws-abc", "--label ade.managed=true", "--label ade.workspace=abc", "--label ade.data_dir=/data",
		"--user 1234:5678", "--cap-drop ALL", "--security-opt no-new-privileges",
		"--mount type=bind,source=/ws/abc,target=/workspace", "--memory 4g", "--cpus 2", "--pids-limit 512",
		"--publish 127.0.0.1::3000", "--env GIT_AUTHOR_NAME=X", "--entrypoint sleep img:1 infinity",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("fehlt %q in %s", want, joined)
		}
	}
	for _, bad := range []string{"--privileged", "docker.sock", "--network host", "--volume"} {
		if strings.Contains(joined, bad) {
			t.Errorf("unerwartet %q", bad)
		}
	}
	if n := strings.Count(joined, "--mount"); n != 1 {
		t.Errorf("%d Mounts, erwartet genau einen", n)
	}
}

func TestExecArgsKeepsValuesOutOfArgv(t *testing.T) {
	m := New(Options{DataDir: "/data", UID: 1000, GID: 1000})
	args, env := m.ExecArgs("abc", Exec{Argv: []string{"claude", "-p"}, Env: []string{"ANTHROPIC_API_KEY=geheim", "PATH=/x"}, TTY: true})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "geheim") {
		t.Fatalf("Secret in der Prozessliste: %s", joined)
	}
	if !slices.Contains(env, "ANTHROPIC_API_KEY=geheim") {
		t.Errorf("Variable fehlt in der Umgebung der CLI")
	}
	for _, want := range []string{"exec --interactive --tty --detach-keys", "--user 1000:1000", "--workdir /workspace", "--env ANTHROPIC_API_KEY ", "--env PATH=/x", "ade-ws-abc claude -p"} {
		if !strings.Contains(joined, want) {
			t.Errorf("fehlt %q in %s", want, joined)
		}
	}
}
