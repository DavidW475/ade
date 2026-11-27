package sandbox

// Container-Laufzeiten (doc/sandbox.md, Abschnitt 6): ADE spricht immer die Docker-CLI-Syntax
// (ADR-014), dahinter kann Docker (rootful oder rootless) oder Podman stehen. Die Laufzeit
// bestimmt, mit welcher UID Prozesse im Container laufen, damit Dateien im Workspace auf dem
// Host dem Nutzer gehören, und welche Optionen und Ressourcen-Grenzen verfügbar sind.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Arten von Container-Laufzeiten.
const (
	RuntimeDocker         = "docker"
	RuntimeDockerRootless = "docker-rootless"
	RuntimePodman         = "podman"
	RuntimePodmanRootful  = "podman-rootful"
)

// Runtime beschreibt die Container-Laufzeit hinter der Docker-CLI.
type Runtime struct {
	// Kind ist eine der Runtime*-Konstanten.
	Kind    string `json:"kind"`
	Version string `json:"version"`
	// Memory, CPU und PIDs melden, ob die Laufzeit die jeweilige Grenze durchsetzen kann
	// (cgroup-Controller vorhanden bzw. an den Nutzer delegiert).
	Memory bool `json:"memory"`
	CPU    bool `json:"cpu"`
	PIDs   bool `json:"pids"`
}

// Name ist die Bezeichnung der Laufzeit für Meldungen.
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

// Podman meldet eine Podman-Laufzeit.
func (r Runtime) Podman() bool { return r.Kind == RuntimePodman || r.Kind == RuntimePodmanRootful }

// RootInContainer meldet, ob Prozesse im Container als root laufen müssen, damit ihre Dateien
// auf dem Host dem Nutzer gehören: Unter rootless Docker ist root im Container der Nutzer auf
// dem Host, jede andere UID landet in dessen subuid-Bereich. Ohne Capabilities hat dieser
// root keine Rechte über die des Nutzers hinaus.
func (r Runtime) RootInContainer() bool { return r.Kind == RuntimeDockerRootless }

// dockerInfo sind die Felder von docker info, die ADE braucht.
type dockerInfo struct {
	ServerVersion   string
	SecurityOptions []string
	MemoryLimit     bool
	CPUCfsQuota     bool `json:"CpuCfsQuota"`
	PidsLimit       bool
	ServerErrors    []string
}

// podmanInfo sind die Felder von podman info.
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

// ParseInfo liest die Ausgabe von „docker info --format '{{json .}}'“ bzw. derselben
// Anfrage an podman (auch über einen docker-Shim von Podman).
func ParseInfo(data []byte) (Runtime, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return Runtime{}, fmt.Errorf("Ausgabe von info lesen: %w", err)
	}
	if _, ok := keys["host"]; ok {
		var p podmanInfo
		if err := json.Unmarshal(data, &p); err != nil {
			return Runtime{}, fmt.Errorf("Ausgabe von podman info lesen: %w", err)
		}
		r := Runtime{Kind: RuntimePodmanRootful, Version: p.Version.Version}
		if p.Host.Security.Rootless {
			r.Kind = RuntimePodman
		}
		if p.Host.CgroupVersion == "v1" && !p.Host.Security.Rootless {
			// cgroups v1 meldet keine Controller-Liste; rootful verwaltet Podman alle.
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
		return Runtime{}, fmt.Errorf("Ausgabe von docker info lesen: %w", err)
	}
	if d.ServerVersion == "" {
		msg := strings.Join(d.ServerErrors, "; ")
		if msg == "" {
			msg = "keine Server-Version"
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

// user ist die UID:GID für Prozesse in den Containern (siehe RootInContainer).
func (m *Manager) user() string {
	if m.runtime().RootInContainer() {
		return "0:0"
	}
	return strconv.Itoa(m.opts.UID) + ":" + strconv.Itoa(m.opts.GID)
}

// userArgs sind die Optionen für docker create, die Nutzer und UID-Abbildung festlegen.
func (m *Manager) userArgs() []string {
	args := []string{"--user", m.user()}
	if m.runtime().Kind == RuntimePodman {
		// Die UID des Nutzers auf dem Host ist im Container dieselbe UID.
		args = append(args, "--userns", "keep-id")
	}
	return args
}

// runtime liefert die zuletzt erkannte Laufzeit (vor der ersten Erkennung: Docker rootful).
func (m *Manager) runtime() Runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rt != nil {
		return *m.rt
	}
	return Runtime{Kind: RuntimeDocker, Memory: true, CPU: true, PIDs: true}
}

// checkResources prüft, ob die Laufzeit Speicher, CPU und Prozesse begrenzen kann. Agent- und
// Proxy-Container haben immer Grenzen (Voreinstellungen bzw. feste Werte); fehlt ein
// cgroup-Controller, verwirft Docker sie teils nur mit Warnung, deshalb lieber gleich abbrechen.
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
	return fmt.Errorf("%s kann die Ressourcen-Grenzen der Sandbox nicht durchsetzen, es fehlt der cgroup-Controller %s "+
		"(rootless muss systemd ihn an den Nutzer delegieren, siehe doc/sandbox.md#6-container-laufzeiten)",
		rt.Name(), strings.Join(missing, ", "))
}

// egressNetworkOpts sind die Optionen des gemeinsamen Egress-Netzes. Podman kennt die
// Docker-Option gegen Verkehr zwischen Containern nicht (Einschränkung, doc/sandbox.md).
func (m *Manager) egressNetworkOpts() []string {
	if m.runtime().Podman() {
		return nil
	}
	return []string{"--opt", "com.docker.network.bridge.enable_icc=false"}
}

// runtimeEnv sind Variablen, die alle Container wegen der Laufzeit brauchen.
func (m *Manager) runtimeEnv() []string {
	if m.runtime().RootInContainer() {
		// Claude Code verweigert bypassPermissions als root außerhalb einer Sandbox.
		return []string{"IS_SANDBOX=1"}
	}
	return nil
}
