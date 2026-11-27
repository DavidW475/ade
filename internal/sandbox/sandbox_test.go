package sandbox

import (
	"os"
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
	if e := c.Egress; e == nil || !e.Default || !slices.Equal(e.Presets, DefaultPresets) || e.OnBlocked != OnBlockedDeny {
		t.Fatalf("Egress ohne Abschnitt: %+v", c.Egress)
	}
	c, err = ParseConfig([]byte("egress:\n  presets: [npm]\n  allow: [api.example.com, \"*.githubusercontent.com\", \"x.example.com:8443\"]\n  deny: [\"*.pastebin.com\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	allow, deny := c.Egress.Rules()
	if c.Egress.Default || !slices.Equal(allow, []string{"registry.npmjs.org", "registry.yarnpkg.com", "api.example.com", "*.githubusercontent.com", "x.example.com:8443"}) ||
		!slices.Equal(deny, []string{"*.pastebin.com"}) {
		t.Fatalf("Egress: %+v → %v %v", c.Egress, allow, deny)
	}
	// Leerer Abschnitt: nichts erlaubt.
	c, err = ParseConfig([]byte("egress: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if allow, _ := c.Egress.Rules(); len(allow) != 0 {
		t.Fatalf("egress: {} erlaubt %v", allow)
	}
	for _, bad := range []string{
		"egress:\n  presets: [unbekannt]\n",
		"egress:\n  allow: [\"*\"]\n",
		"egress:\n  deny: [\"http://x\"]\n",
		"egress:\n  on_blocked: ask\n",
		"egress:\n  on_blocked: vielleicht\n",
		"egress:\n  bogus: 1\n",
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
		"--network ade-ws-abc", "--network-alias agent", "--dns 192.0.2.1", "--label ade.net=" + NetVersion,
		"--env HTTPS_PROXY=http://proxy:8080", "--env https_proxy=http://proxy:8080", "--env NO_PROXY=localhost",
		"--env GIT_AUTHOR_NAME=X", "--entrypoint sleep img:1 infinity",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("fehlt %q in %s", want, joined)
		}
	}
	for _, bad := range []string{"--privileged", "docker.sock", "--network host", "--volume", "--publish", "ANTHROPIC"} {
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

func TestProxyCreateArgs(t *testing.T) {
	m := New(Options{DataDir: "/data", Image: "img:1", ProxyBinary: "/data/sandbox/ade-proxy", UID: 1234, GID: 5678})
	joined := strings.Join(m.ProxyCreateArgs("abc"), " ")
	for _, want := range []string{
		"--name ade-proxy-abc", "--label ade.role=proxy", "--label ade.workspace=abc", "--label ade.data_dir=/data",
		"--network ade-egress", "--user 1234:5678", "--cap-drop ALL", "--security-opt no-new-privileges", "--read-only",
		"--mount type=bind,source=/data/sandbox/ade-proxy,target=/opt/ade/ade,readonly",
		"--publish 127.0.0.1::8081", "--publish 127.0.0.1::3000",
		"--env ADE_PROXY_TOKEN --publish", // ohne Wert: kommt aus der Umgebung der CLI
		"--entrypoint /opt/ade/ade img:1 proxy --listen :8080 --control :8081 --ide :3000 --ide-target agent:3000",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("fehlt %q in %s", want, joined)
		}
	}
	for _, bad := range []string{"--privileged", "docker.sock", "--network host", "0.0.0.0", "/workspace"} {
		if strings.Contains(joined, bad) {
			t.Errorf("unerwartet %q", bad)
		}
	}
}

func TestPolicyFor(t *testing.T) {
	m := New(Options{DataDir: "/data", Secrets: func() map[string]string { return map[string]string{"anthropic": "sk"} }})
	p := m.policyFor(*withDefaults(&Config{}))
	if !slices.Contains(p.Allow, "registry.npmjs.org") || !slices.Contains(p.Allow, "api.anthropic.com") || p.Secrets["anthropic"] != "sk" {
		t.Fatalf("Policy: %+v", p)
	}
	c, err := ParseConfig([]byte("egress:\n  allow: [a.example]\n"))
	if err != nil {
		t.Fatal(err)
	}
	p2 := m.policyFor(*c)
	if !slices.Equal(p2.Allow, []string{"a.example"}) {
		t.Fatalf("Policy: %+v", p2)
	}
	if policyHash(p) == policyHash(p2) {
		t.Fatal("verschiedene Policies mit gleichem Hash")
	}
	p3 := p2
	p3.Secrets = map[string]string{"anthropic": "anderer"}
	if policyHash(p2) == policyHash(p3) {
		t.Fatal("geänderter Key ändert den Hash nicht")
	}
}

func TestInstallProxyBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Das Test-Binary ist je nach Umgebung dynamisch gelinkt; dann muss die Prüfung greifen.
	dst, err := InstallProxyBinary(exe, dir)
	if err != nil {
		if !strings.Contains(err.Error(), "CGO_ENABLED=0") {
			t.Fatal(err)
		}
		return
	}
	st, err := os.Stat(dst)
	if err != nil || st.Mode().Perm() != 0o500 {
		t.Fatalf("%s: %v %v", dst, st, err)
	}
	if again, err := InstallProxyBinary(exe, dir); err != nil || again != dst {
		t.Fatalf("zweiter Aufruf: %s %v", again, err)
	}
}
