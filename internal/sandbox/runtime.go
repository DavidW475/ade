package sandbox

// Container runtimes (doc/sandbox.md, section 6): ADE always speaks the Docker CLI syntax
// (ADR-014); behind it can be Docker (rootful or rootless) or Podman. The runtime determines
// with which UID processes run in the container, so that files in the workspace belong to the
// user on the host, and which options and resource limits are available.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Kinds of container runtimes.
const (
	RuntimeDocker         = "docker"
	RuntimeDockerRootless = "docker-rootless"
	RuntimePodman         = "podman"
	RuntimePodmanRootful  = "podman-rootful"
)

// Runtime describes the container runtime behind the Docker CLI.
type Runtime struct {
	// Kind is one of the Runtime* constants.
	Kind    string `json:"kind"`
	Version string `json:"version"`
	// Memory, CPU and PIDs report whether the runtime can enforce the respective limit
	// (cgroup controller present or delegated to the user).
	Memory bool `json:"memory"`
	CPU    bool `json:"cpu"`
	PIDs   bool `json:"pids"`
}

// Name is the runtime's label for messages.
func (r Runtime) Name() string {
	switch r.Kind {
	case RuntimeDockerRootless:
		return "Docker (rootless)"
	case RuntimePodman:
		return "Podman (rootless)"
	case RuntimePodmanRootful:
		return "Podman (rootful)"
	}
	return "Docker"
}

// Podman reports a Podman runtime.
func (r Runtime) Podman() bool { return r.Kind == RuntimePodman || r.Kind == RuntimePodmanRootful }

// RootInContainer reports whether processes in the container must run as root so that their
// files belong to the user on the host: under rootless Docker, root in the container is the
// user on the host, any other UID ends up in their subuid range. Without capabilities this
// root has no rights beyond the user's.
func (r Runtime) RootInContainer() bool { return r.Kind == RuntimeDockerRootless }

// dockerInfo are the fields of docker info that ADE needs.
type dockerInfo struct {
	ServerVersion   string
	SecurityOptions []string
	MemoryLimit     bool
	CPUCfsQuota     bool `json:"CpuCfsQuota"`
	PidsLimit       bool
	ServerErrors    []string
}

// podmanInfo are the fields of podman info.
type podmanInfo struct {
	Host struct {
		Security struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
		CgroupVersion     string   `json:"cgroupVersion"`
		CgroupControllers []string `json:"cgroupControllers"`
	} `json:"host"`
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
}

// ParseInfo reads the output of “docker info --format '{{json .}}'” or the same request to
// podman (also through a docker shim of Podman).
func ParseInfo(data []byte) (Runtime, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return Runtime{}, fmt.Errorf("read output of info: %w", err)
	}
	if _, ok := keys["host"]; ok {
		var p podmanInfo
		if err := json.Unmarshal(data, &p); err != nil {
			return Runtime{}, fmt.Errorf("read output of podman info: %w", err)
		}
		r := Runtime{Kind: RuntimePodmanRootful, Version: p.Version.Version}
		if p.Host.Security.Rootless {
			r.Kind = RuntimePodman
		}
		if p.Host.CgroupVersion == "v1" && !p.Host.Security.Rootless {
			// cgroups v1 reports no controller list; rootful Podman manages all of them.
			r.Memory, r.CPU, r.PIDs = true, true, true
		} else {
			r.Memory = slices.Contains(p.Host.CgroupControllers, "memory")
			r.CPU = slices.Contains(p.Host.CgroupControllers, "cpu")
			r.PIDs = slices.Contains(p.Host.CgroupControllers, "pids")
		}
		return r, nil
	}
	var d dockerInfo
	if err := json.Unmarshal(data, &d); err != nil {
		return Runtime{}, fmt.Errorf("read output of docker info: %w", err)
	}
	if d.ServerVersion == "" {
		msg := strings.Join(d.ServerErrors, "; ")
		if msg == "" {
			msg = "no server version"
		}
		return Runtime{}, errors.New(msg)
	}
	r := Runtime{Kind: RuntimeDocker, Version: d.ServerVersion, Memory: d.MemoryLimit, CPU: d.CPUCfsQuota, PIDs: d.PidsLimit}
	for _, o := range d.SecurityOptions {
		if o == "name=rootless" {
			r.Kind = RuntimeDockerRootless
		}
	}
	return r, nil
}

// user is the UID:GID for processes in the containers (see RootInContainer).
func (m *Manager) user() string {
	if m.runtime().RootInContainer() {
		return "0:0"
	}
	return strconv.Itoa(m.opts.UID) + ":" + strconv.Itoa(m.opts.GID)
}

// userArgs are the options for docker create that set the user and the UID mapping.
func (m *Manager) userArgs() []string {
	args := []string{"--user", m.user()}
	if m.runtime().Kind == RuntimePodman {
		// The user's UID on the host is the same UID in the container.
		args = append(args, "--userns", "keep-id")
	}
	return args
}

// runtime returns the most recently detected runtime (before the first detection: Docker rootful).
func (m *Manager) runtime() Runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rt != nil {
		return *m.rt
	}
	return Runtime{Kind: RuntimeDocker, Memory: true, CPU: true, PIDs: true}
}

// checkResources checks whether the runtime can limit memory, CPU and processes. Agent and
// proxy containers always have limits (defaults or fixed values); if a cgroup controller is
// missing, Docker sometimes drops them with only a warning, so better abort right away.
func (m *Manager) checkResources() error {
	rt := m.runtime()
	var missing []string
	for _, c := range []struct {
		ok   bool
		name string
	}{{rt.Memory, "memory"}, {rt.CPU, "cpu"}, {rt.PIDs, "pids"}} {
		if !c.ok {
			missing = append(missing, c.name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s cannot enforce the sandbox's resource limits, the cgroup controller %s is missing "+
		"(rootless, systemd must delegate it to the user, see doc/sandbox.md#6-container-runtimes)",
		rt.Name(), strings.Join(missing, ", "))
}

// egressNetworkOpts are the options of the shared egress network. Podman does not know the
// Docker option against traffic between containers (limitation, doc/sandbox.md).
func (m *Manager) egressNetworkOpts() []string {
	if m.runtime().Podman() {
		return nil
	}
	return []string{"--opt", "com.docker.network.bridge.enable_icc=false"}
}

// runtimeEnv are variables all containers need because of the runtime.
func (m *Manager) runtimeEnv() []string {
	if m.runtime().RootInContainer() {
		// Claude Code refuses bypassPermissions as root outside a sandbox.
		return []string{"IS_SANDBOX=1"}
	}
	return nil
}
