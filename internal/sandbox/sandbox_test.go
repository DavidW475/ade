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
		t.Fatalf("empty: %+v, %v", c, err)
	}
	if e := c.Egress; e == nil || !e.Default || !slices.Equal(e.Presets, DefaultPresets) || e.OnBlocked != OnBlockedDeny {
		t.Fatalf("egress without a section: %+v", c.Egress)
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
	// Empty section: nothing allowed.
	c, err = ParseConfig([]byte("egress: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if allow, _ := c.Egress.Rules(); len(allow) != 0 {
		t.Fatalf("egress: {} allows %v", allow)
	}
	for _, bad := range []string{
		"egress:\n  presets: [unknown]\n",
		"egress:\n  allow: [\"*\"]\n",
		"egress:\n  deny: [\"http://x\"]\n",
		"egress:\n  on_blocked: vielleicht\n",
		"egress:\n  bogus: 1\n",
		"image: \"-v /:/host\"\n",
		"resources:\n  memory: viel\n",
		"resources:\n  cpus: -1\n",
		"resources:\n  pids: 3\n",
		"unknown: 1\n",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestDefaultImage(t *testing.T) {
	for v, want := range map[string]string{"v1.2.3": "1.2.3", "v1.0.0-rc.1": "1.0.0-rc.1", "v2.0.0-beta.12": "2.0.0-beta.12"} {
		if got := DefaultImage(v); got != ImageRepo+":"+want {
			t.Errorf("%q: %s", v, got)
		}
	}
	for _, v := range []string{"dev", "", "1.2.3", "v1.2.3-4-gabc-dirty", "v1.0.0-rc.1-2-gabc", "v1.0.0-SNAPSHOT-abc1234", "v1.0.0-rc1"} {
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
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	for _, bad := range []string{"--privileged", "docker.sock", "--network host", "--volume", "--publish", "ANTHROPIC"} {
		if strings.Contains(joined, bad) {
			t.Errorf("unerwartet %q", bad)
		}
	}
	if n := strings.Count(joined, "--mount"); n != 1 {
		t.Errorf("%d mounts, want exactly one", n)
	}

	// With proxy binary: additionally read-only for the approval hook.
	m = New(Options{DataDir: "/data", Image: "img:1", ProxyBinary: "/data/sandbox/ade-proxy"})
	joined = strings.Join(m.CreateArgs(Spec{Workspace: "abc", Path: "/ws/abc", Config: *withDefaults(&Config{})}), " ")
	if !strings.Contains(joined, "--mount type=bind,source=/data/sandbox/ade-proxy,target=/opt/ade/ade,readonly") || strings.Count(joined, "--mount") != 2 {
		t.Errorf("Hook-Binary: %s", joined)
	}
}

func TestReaderCreateArgs(t *testing.T) {
	m := New(Options{DataDir: "/data", Image: "img:1", UID: 1234, GID: 5678})
	joined := strings.Join(m.ReaderCreateArgs(Spec{Workspace: "abc", Path: "/ws/abc", Config: *withDefaults(&Config{})}), " ")
	for _, want := range []string{
		"--name ade-ws-abc-ro", "--label ade.role=reader", "--label ade.workspace=abc",
		"--mount type=bind,source=/ws/abc,target=/workspace,readonly", "--network ade-ws-abc", "--network-alias reader",
		"--cap-drop ALL", "--user 1234:5678",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "--network-alias agent") || strings.Count(joined, "--mount") != 1 {
		t.Errorf("Lese-Container: %s", joined)
	}
	args, _ := m.ExecArgs("abc", Exec{Argv: []string{"true"}, Reader: true})
	if !slices.Contains(args, "ade-ws-abc-ro") || slices.Contains(args, "ade-ws-abc") {
		t.Errorf("exec in the reader container: %v", args)
	}
}

func TestExecArgsKeepsValuesOutOfArgv(t *testing.T) {
	m := New(Options{DataDir: "/data", UID: 1000, GID: 1000})
	args, env := m.ExecArgs("abc", Exec{Argv: []string{"claude", "-p"}, Env: []string{"ANTHROPIC_API_KEY=geheim", "PATH=/x"}, TTY: true})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "geheim") {
		t.Fatalf("secret in the process list: %s", joined)
	}
	if !slices.Contains(env, "ANTHROPIC_API_KEY=geheim") {
		t.Errorf("variable missing in the CLI's environment")
	}
	for _, want := range []string{"exec --interactive --tty --detach-keys", "--user 1000:1000", "--workdir /workspace", "--env ANTHROPIC_API_KEY ", "--env PATH=/x", "ade-ws-abc claude -p"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
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
		"--env ADE_PROXY_TOKEN --publish", // without a value: comes from the CLI's environment
		"--entrypoint /opt/ade/ade img:1 proxy --listen :8080 --control :8081 --ide :3000 --ide-target agent:3000",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
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
	p := m.policyFor(Spec{Config: *withDefaults(&Config{})})
	if !slices.Contains(p.Allow, "registry.npmjs.org") || !slices.Contains(p.Allow, "api.anthropic.com") || p.Secrets["anthropic"] != "sk" {
		t.Fatalf("Policy: %+v", p)
	}
	c, err := ParseConfig([]byte("egress:\n  allow: [a.example]\n"))
	if err != nil {
		t.Fatal(err)
	}
	p2 := m.policyFor(Spec{Config: *c})
	if !slices.Equal(p2.Allow, []string{"a.example"}) || p2.Ask {
		t.Fatalf("Policy: %+v", p2)
	}
	if policyHash(p) == policyHash(p2) {
		t.Fatal("different policies with the same hash")
	}
	p3 := p2
	p3.Secrets = map[string]string{"anthropic": "anderer"}
	if policyHash(p2) == policyHash(p3) {
		t.Fatal("changed key does not change the hash")
	}

	// Exceptions from approvals extend the allowlist; on_blocked: ask enables questions.
	c, err = ParseConfig([]byte("egress:\n  allow: [a.example]\n  on_blocked: ask\n"))
	if err != nil {
		t.Fatal(err)
	}
	p4 := m.policyFor(Spec{Config: *c, Grants: []string{"a.example", "pypi.org"}})
	if !slices.Equal(p4.Allow, []string{"a.example", "pypi.org"}) || !p4.Ask || p4.AskWaitSeconds != 120 {
		t.Fatalf("Policy: %+v", p4)
	}
	if policyHash(p4) == policyHash(m.policyFor(Spec{Config: *c})) {
		t.Fatal("exception does not change the hash")
	}
	if _, err := ParseConfig([]byte("egress:\n  on_blocked: vielleicht\n")); err == nil {
		t.Fatal("invalid on_blocked accepted")
	}
}

func TestInstallProxyBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Depending on the environment the test binary is dynamically linked; then the check must apply.
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
		t.Fatalf("second call: %s %v", again, err)
	}
}
