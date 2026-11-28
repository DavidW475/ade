package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shortened outputs of “info --format '{{json .}}'”.
const (
	infoDocker         = `{"ServerVersion":"29.0.1","SecurityOptions":["name=seccomp,profile=builtin","name=cgroupns"],"MemoryLimit":true,"CpuCfsQuota":true,"PidsLimit":true}`
	infoDockerRootless = `{"ServerVersion":"29.0.1","SecurityOptions":["name=seccomp,profile=builtin","name=rootless","name=cgroupns"],"MemoryLimit":true,"CpuCfsQuota":false,"PidsLimit":true}`
	infoDockerDown     = `{"ServerVersion":"","ServerErrors":["Cannot connect to the Docker daemon at unix:///var/run/docker.sock"]}`
	infoPodman         = `{"host":{"cgroupVersion":"v2","cgroupControllers":["cpu","io","memory","pids"],"security":{"rootless":true}},"version":{"APIVersion":"5.7.0","Version":"5.7.0"}}`
	infoPodmanV1       = `{"host":{"cgroupVersion":"v1","cgroupControllers":[],"security":{"rootless":true}},"version":{"Version":"4.9.3"}}`
	infoPodmanRootful  = `{"host":{"cgroupVersion":"v1","cgroupControllers":[],"security":{"rootless":false}},"version":{"Version":"4.9.3"}}`
)

func TestParseInfo(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     Runtime
	}{
		{"docker", infoDocker, Runtime{Kind: RuntimeDocker, Version: "29.0.1", Memory: true, CPU: true, PIDs: true}},
		{"docker rootless", infoDockerRootless, Runtime{Kind: RuntimeDockerRootless, Version: "29.0.1", Memory: true, PIDs: true}},
		{"podman", infoPodman, Runtime{Kind: RuntimePodman, Version: "5.7.0", Memory: true, CPU: true, PIDs: true}},
		{"podman cgroups v1", infoPodmanV1, Runtime{Kind: RuntimePodman, Version: "4.9.3"}},
		{"podman rootful", infoPodmanRootful, Runtime{Kind: RuntimePodmanRootful, Version: "4.9.3", Memory: true, CPU: true, PIDs: true}},
	} {
		got, err := ParseInfo([]byte(tc.in))
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
	if _, err := ParseInfo([]byte(infoDockerDown)); err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Errorf("Docker not reachable: %v", err)
	}
	if _, err := ParseInfo([]byte("no json")); err == nil {
		t.Error("invalid output without an error")
	}
}

func managerFor(rt Runtime) *Manager {
	return New(Options{DataDir: "/data", UID: 1234, GID: 5678, ProxyBinary: "/data/sandbox/ade", Runtime: &rt})
}

// #94: files the agent creates belong to the user on the host.
func TestUserMappingPerRuntime(t *testing.T) {
	spec := Spec{Workspace: "abc", Path: "/ws/abc", Config: *withDefaults(&Config{})}
	for _, tc := range []struct {
		kind       string
		want, deny []string
	}{
		{RuntimeDocker, []string{"--user 1234:5678"}, []string{"--userns", "IS_SANDBOX"}},
		// root in the container = the user on the host.
		{RuntimeDockerRootless, []string{"--user 0:0", "--env IS_SANDBOX=1"}, []string{"--userns", "1234"}},
		// keep-id: same UID in the container and on the host.
		{RuntimePodman, []string{"--user 1234:5678 --userns keep-id"}, []string{"IS_SANDBOX"}},
		{RuntimePodmanRootful, []string{"--user 1234:5678"}, []string{"--userns"}},
	} {
		m := managerFor(Runtime{Kind: tc.kind, Memory: true, CPU: true, PIDs: true})
		for name, args := range map[string][]string{"agent": m.CreateArgs(spec), "reader": m.ReaderCreateArgs(spec), "proxy": m.ProxyCreateArgs("abc")} {
			joined := strings.Join(args, " ")
			for _, w := range tc.want {
				if name == "proxy" && strings.HasPrefix(w, "--env") {
					continue
				}
				if !strings.Contains(joined, w) {
					t.Errorf("%s/%s: %q missing in %s", tc.kind, name, w, joined)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(joined, d) {
					t.Errorf("%s/%s: %q in %s", tc.kind, name, d, joined)
				}
			}
		}
		execArgs, _ := m.ExecArgs("abc", Exec{Argv: []string{"id"}})
		wantUser := "--user 1234:5678"
		if tc.kind == RuntimeDockerRootless {
			wantUser = "--user 0:0"
		}
		if !strings.Contains(strings.Join(execArgs, " "), wantUser) {
			t.Errorf("%s: exec %v", tc.kind, execArgs)
		}
	}
}

func TestEgressNetworkOpts(t *testing.T) {
	if got := managerFor(Runtime{Kind: RuntimeDocker}).egressNetworkOpts(); len(got) != 2 {
		t.Errorf("Docker: %v", got)
	}
	if got := managerFor(Runtime{Kind: RuntimePodman}).egressNetworkOpts(); len(got) != 0 {
		t.Errorf("Podman: %v", got)
	}
}

func TestCheckResources(t *testing.T) {
	if err := managerFor(Runtime{Kind: RuntimeDocker, Memory: true, CPU: true, PIDs: true}).checkResources(); err != nil {
		t.Fatal(err)
	}
	err := managerFor(Runtime{Kind: RuntimeDockerRootless, Memory: true, PIDs: true}).checkResources()
	if err == nil || !strings.Contains(err.Error(), "Docker (rootless)") || !strings.Contains(err.Error(), "cgroup controller cpu") {
		t.Fatalf("missing cpu: %v", err)
	}
}

// The runtime is detected during Ensure; if controllers are missing, the start fails with a
// clear message before a container is created.
func TestEnsureFailsEarlyWithoutControllers(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "docker")
	log := filepath.Join(dir, "aufrufe")
	body := `#!/bin/sh
echo "$*" >> ` + log + `
case "$1" in
  info) echo '` + infoPodmanV1 + `' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // Testskript
		t.Fatal(err)
	}
	m := New(Options{Docker: script, DataDir: dir, ProxyBinary: "/x/ade"})
	defer m.Close()
	err := m.Ensure(context.Background(), Spec{Workspace: "w", Path: dir, Config: *withDefaults(&Config{})})
	if err == nil || !strings.Contains(err.Error(), "Podman (rootless)") || !strings.Contains(err.Error(), "memory, cpu, pids") {
		t.Fatalf("Ensure: %v", err)
	}
	if rt, ok := m.Runtime(); !ok || rt.Kind != RuntimePodman {
		t.Errorf("Runtime = %+v, %v", rt, ok)
	}
	calls, _ := os.ReadFile(log) //nolint:gosec // Testdatei
	if strings.Contains(string(calls), "create") {
		t.Errorf("container created despite missing controllers:\n%s", calls)
	}
}
