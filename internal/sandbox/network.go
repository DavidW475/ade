package sandbox

// Network and egress proxy of a workspace (doc/sandbox.md, sections 1 and 3):
//
//	ade-ws-<id>    Docker network with --internal: no route to the outside
//	ade-ws-<id>    agent container, only in this network (alias agent)
//	ade-proxy-<id> proxy container (ade proxy), in ade-egress and ade-ws-<id> (alias proxy)
//	ade-egress     shared network of all proxies with internet, without traffic between containers
//
// Docker does not publish ports of containers in internal networks. The daemon therefore
// reaches the control API and the IDE through ports of the proxy container on 127.0.0.1; the
// proxy forwards the IDE to agent:3000.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DavidW475/ade/internal/proxy"
)

// Names and values of the sandbox network.
const (
	EgressNetwork = "ade-egress"
	// ProxyAlias is the name of the proxy in the workspace network.
	ProxyAlias = "proxy"
	// AgentAlias is the name of the agent container in the workspace network.
	AgentAlias = "agent"
	// NetVersion marks the layout of the containers (proxy-1: network from M6, proxy-2: plus
	// the ade binary for the approval hook, M7); older ones are recreated on the next start.
	NetVersion = "proxy-2"
	// ProxyBinary is the path of the ade binary in the proxy and the agent container.
	ProxyBinary = "/opt/ade/ade"
	// noDNS is the agent container's DNS server for external names: an address from
	// TEST-NET-1 that is not reachable in the internal network. Docker's embedded DNS still
	// resolves container names (proxy); only the proxy resolves external names (#57).
	noDNS = "192.0.2.1"
)

// ProxyURL is the proxy's address from the agent container's point of view.
var ProxyURL = "http://" + ProxyAlias + ":" + strconv.Itoa(proxy.Port)

// ProviderBaseURL is the base URL of the secret injection for a provider.
func ProviderBaseURL(provider string) string { return ProxyURL + "/p/" + provider }

// NetworkName is the name of a workspace's internal network.
func NetworkName(workspace string) string { return "ade-ws-" + workspace }

// ProxyName is the name of a workspace's proxy container.
func ProxyName(workspace string) string { return "ade-proxy-" + workspace }

// proxyEnv are the proxy variables in the agent container (a convenience – the isolation is
// enforced by the internal network).
func proxyEnv() []string {
	// address the proxy itself directly (base URLs of the secret injection).
	noProxy := "localhost,127.0.0.1,::1," + ProxyAlias
	return []string{
		"HTTP_PROXY=" + ProxyURL, "HTTPS_PROXY=" + ProxyURL, "http_proxy=" + ProxyURL, "https_proxy=" + ProxyURL,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
	}
}

// ProxyCreateArgs returns the arguments for docker create of the proxy container (exported
// for tests). The token comes through the CLI's environment, not through the arguments.
func (m *Manager) ProxyCreateArgs(workspace string) []string {
	args := []string{
		"container", "create",
		"--name", ProxyName(workspace),
		"--label", LabelManaged + "=true",
		"--label", LabelWorkspace + "=" + workspace,
		"--label", LabelDataDir + "=" + m.opts.DataDir,
		"--label", LabelRole + "=" + RoleProxy,
		"--label", LabelNet + "=" + NetVersion,
		"--label", LabelBinary + "=" + m.opts.ProxyBinary,
		"--hostname", ProxyAlias,
		"--network", EgressNetwork,
	}
	// Same UID mapping as the agent container: the mounted binary belongs to the user and is
	// only readable by them.
	args = append(args, m.userArgs()...)
	return append(args,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--init",
		"--memory", "256m",
		"--cpus", "1",
		"--pids-limit", "256",
		"--mount", "type=bind,source="+m.opts.ProxyBinary+",target="+ProxyBinary+",readonly",
		"--env", proxy.EnvToken,
		"--publish", "127.0.0.1::"+strconv.Itoa(proxy.ControlPort),
		"--publish", "127.0.0.1::"+strconv.Itoa(proxy.IDEPort),
		"--entrypoint", ProxyBinary,
		m.opts.ProxyImage,
		"proxy",
		"--listen", ":"+strconv.Itoa(proxy.Port),
		"--control", ":"+strconv.Itoa(proxy.ControlPort),
		"--ide", ":"+strconv.Itoa(proxy.IDEPort),
		"--ide-target", AgentAlias+":"+strconv.Itoa(IDEPort),
	)
}

// ensureNetworks creates the egress network and the workspace's internal network if needed.
// created reports a newly created workspace network.
func (m *Manager) ensureNetworks(ctx context.Context, workspace string) (created bool, err error) {
	if _, err := m.run(ctx, nil, "network", "inspect", "--format", "{{.Name}}", EgressNetwork); err != nil {
		if !isNoSuch(err) {
			return false, fmt.Errorf("check egress network: %w", err)
		}
		args := append([]string{"network", "create", "--driver", "bridge", "--label", LabelManaged + "=true"}, m.egressNetworkOpts()...)
		_, err := m.run(ctx, nil, append(args, EgressNetwork)...)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			if strings.Contains(err.Error(), "bridge-nf-call-iptables") {
				// rootless Docker needs the module for enable_icc=false.
				return false, fmt.Errorf("create egress network: kernel module br_netfilter missing (sudo modprobe br_netfilter, "+
					"see doc/sandbox.md#6-container-runtimes): %w", err)
			}
			return false, fmt.Errorf("create egress network: %w", err)
		}
	}
	out, err := m.run(ctx, nil, "network", "inspect", "--format", `{{.Internal}} {{index .Labels "`+LabelDataDir+`"}}`, NetworkName(workspace))
	if err == nil {
		internal, dataDir, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
		if internal != "true" || dataDir != m.opts.DataDir {
			return false, fmt.Errorf("network %s is not the internal network of this ADE instance", NetworkName(workspace))
		}
		return false, nil
	}
	if !isNoSuch(err) {
		return false, fmt.Errorf("check workspace network: %w", err)
	}
	if _, err := m.run(ctx, nil, "network", "create", "--driver", "bridge", "--internal",
		"--label", LabelManaged+"=true",
		"--label", LabelWorkspace+"="+workspace,
		"--label", LabelDataDir+"="+m.opts.DataDir,
		NetworkName(workspace)); err != nil {
		return false, fmt.Errorf("create workspace network: %w", err)
	}
	return true, nil
}

func isNoSuch(err error) bool {
	var de *Error
	if !errors.As(err, &de) {
		return false
	}
	s := strings.ToLower(de.Stderr)
	return strings.Contains(s, "no such") || strings.Contains(s, "not found")
}

// getProxy returns a workspace's proxy container or nil.
func (m *Manager) getProxy(ctx context.Context, workspace string) (*Container, error) {
	cs, err := m.inspect(ctx, ProxyName(workspace))
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	c := cs[0]
	if c.DataDir != m.opts.DataDir || c.Workspace != workspace || c.Role != RoleProxy {
		return nil, fmt.Errorf("container %s does not belong to this ADE instance", c.Name)
	}
	return &c, nil
}

// ensureProxy creates the proxy container (if needed) and starts it.
func (m *Manager) ensureProxy(ctx context.Context, workspace string, recreate bool) (*Container, error) {
	if m.opts.ProxyBinary == "" {
		return nil, errors.New("no ade binary configured for the egress proxy")
	}
	p, err := m.getProxy(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if p != nil && (recreate || p.Net != NetVersion || p.Binary != m.opts.ProxyBinary) {
		if _, err := m.run(ctx, nil, "container", "rm", "--force", "--", p.ID); err != nil {
			return nil, fmt.Errorf("replace proxy container: %w", err)
		}
		p = nil
	}
	if p == nil {
		if strings.ContainsAny(m.opts.ProxyBinary, ",\n") || !filepath.IsAbs(m.opts.ProxyBinary) {
			return nil, fmt.Errorf("path %q of the ade binary is not suitable for a mount", m.opts.ProxyBinary)
		}
		token := randomToken()
		cmd := m.command(ctx, m.ProxyCreateArgs(workspace)...)
		cmd.Env = append(cmd.Env, proxy.EnvToken+"="+token)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("create proxy container: %w", &Error{Args: []string{"create"}, Stderr: stderr.String(), Err: err})
		}
		if _, err := m.run(ctx, nil, "network", "connect", "--alias", ProxyAlias, NetworkName(workspace), ProxyName(workspace)); err != nil {
			return nil, fmt.Errorf("connect proxy to the workspace network: %w", err)
		}
		m.log.Info("egress proxy created", "workspace", workspace)
		if p, err = m.getProxy(ctx, workspace); err != nil || p == nil {
			return nil, fmt.Errorf("proxy container not found after creating it: %w", err)
		}
	}
	if !p.Running {
		if _, err := m.run(ctx, nil, "container", "start", ProxyName(workspace)); err != nil {
			return nil, fmt.Errorf("start proxy container: %w", err)
		}
	}
	return p, nil
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// proxyLink is the daemon's connection to a proxy's control API.
type proxyLink struct {
	client *proxy.Client
	hash   string // most recently sent policy
	// reloadErr is the last error during hot reload (report it only once).
	reloadErr string
	cancel    context.CancelFunc
	done      chan struct{}
}

// policyFor builds a workspace's policy from its configuration, the exceptions from
// approvals and the secrets.
func (m *Manager) policyFor(s Spec) proxy.Policy {
	e := s.Config.Egress
	if e == nil {
		e = defaultEgress()
	}
	allow, deny := e.Rules()
	for _, g := range s.Grants {
		if !slices.Contains(allow, g) {
			allow = append(allow, g)
		}
	}
	p := proxy.Policy{Allow: allow, Deny: deny}
	if e.OnBlocked == OnBlockedAsk {
		p.Ask, p.AskWaitSeconds = true, int(m.opts.AskWait/time.Second)
	}
	if m.opts.Secrets != nil {
		p.Secrets = m.opts.Secrets()
	}
	return p
}

func policyHash(p proxy.Policy) string {
	keys := make([]string, 0, len(p.Secrets))
	for k := range p.Secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	_ = json.NewEncoder(h).Encode([]any{p.Allow, p.Deny, keys, p.Ask, p.AskWaitSeconds})
	for _, k := range keys {
		_, _ = io.WriteString(h, p.Secrets[k]+"\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// connectProxy connects to the proxy's control API, sends the policy and starts fetching
// the events.
func (m *Manager) connectProxy(ctx context.Context, workspace string, pol proxy.Policy) error {
	p, err := m.getProxy(ctx, workspace)
	if err != nil || p == nil {
		return fmt.Errorf("proxy container: %w", err)
	}
	port, err := m.hostPort(ctx, ProxyName(workspace), proxy.ControlPort)
	if err != nil {
		return err
	}
	link := &proxyLink{client: &proxy.Client{Base: "http://127.0.0.1:" + strconv.Itoa(port), Token: p.token, HTTP: m.http}}
	// The proxy needs a moment after the start.
	deadline := time.Now().Add(15 * time.Second)
	for {
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err = link.client.Health(hctx)
		cancel()
		if err == nil || proxy.IsUnauthorized(err) || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("egress proxy does not answer: %w", err)
	}
	if err := link.client.SetPolicy(ctx, pol); err != nil {
		return fmt.Errorf("send policy to the egress proxy: %w", err)
	}
	link.hash = policyHash(pol)
	cctx, cancel := context.WithCancel(m.bg)
	link.cancel, link.done = cancel, make(chan struct{})
	m.mu.Lock()
	old := m.links[workspace]
	m.links[workspace] = link
	m.mu.Unlock()
	if old != nil {
		old.cancel()
	}
	go func() {
		defer close(link.done)
		var wg sync.WaitGroup
		wg.Go(func() { m.answerQuestions(cctx, workspace, link) })
		m.collect(cctx, workspace, link)
		cancel() // proxy gone: also stop fetching the questions
		wg.Wait()
	}()
	return nil
}

// unlink stops fetching a workspace's events.
func (m *Manager) unlink(workspace string) {
	m.mu.Lock()
	link := m.links[workspace]
	delete(m.links, workspace)
	m.mu.Unlock()
	if link != nil {
		link.cancel()
		<-link.done
	}
}

// collect fetches events by long polling until ctx ends or the proxy is gone for good.
func (m *Manager) collect(ctx context.Context, workspace string, link *proxyLink) {
	defer func() {
		m.mu.Lock()
		if m.links[workspace] == link {
			delete(m.links, workspace)
			delete(m.ensure, workspace)
		}
		m.mu.Unlock()
	}()
	var boot string
	var after int64
	failures := 0
	for ctx.Err() == nil {
		r, err := link.client.Events(ctx, boot, after, 20*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			if failures >= 5 {
				m.log.Debug("egress proxy no longer reachable", "workspace", workspace, "err", err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(failures) * time.Second):
			}
			continue
		}
		failures = 0
		if r.Boot != boot {
			boot = r.Boot
		}
		after = r.Next
		m.mu.Lock()
		sink := m.opts.OnEgress
		m.mu.Unlock()
		if len(r.Events) > 0 && sink != nil {
			sink(workspace, r.Events)
		}
	}
}

// QuestionHandler answers a question of a proxy (see Options.OnQuestion).
type QuestionHandler func(ctx context.Context, workspace string, q proxy.Question) proxy.Answer

// SetQuestionHandler sets the handler for questions of the proxies.
func (m *Manager) SetQuestionHandler(h QuestionHandler) {
	m.mu.Lock()
	m.opts.OnQuestion = h
	m.mu.Unlock()
}

// answerQuestions fetches open questions by long polling and answers each in its own
// goroutine. If a question disappears at the proxy (the asker gave up), its handler's
// context ends.
func (m *Manager) answerQuestions(ctx context.Context, workspace string, link *proxyLink) {
	type running struct{ cancel context.CancelFunc }
	active := map[string]running{}
	var boot string
	var version int64
	failures := 0
	var wg sync.WaitGroup
	defer func() {
		for _, r := range active {
			r.cancel()
		}
		wg.Wait()
	}()
	for ctx.Err() == nil {
		r, err := link.client.Questions(ctx, boot, version, 20*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(min(failures, 10)) * time.Second):
			}
			continue
		}
		failures = 0
		boot, version = r.Boot, r.Version
		open := map[string]bool{}
		for _, q := range r.Questions {
			open[q.ID] = true
			if _, ok := active[q.ID]; ok {
				continue
			}
			qctx, cancel := context.WithCancel(ctx)
			active[q.ID] = running{cancel}
			wg.Go(func() {
				defer cancel()
				m.mu.Lock()
				h := m.opts.OnQuestion
				m.mu.Unlock()
				a := proxy.Answer{Decision: proxy.AnswerDeny, Reason: "ADE: approvals are not set up"}
				if h != nil {
					a = h(qctx, workspace, q)
				}
				if qctx.Err() != nil {
					return
				}
				actx, acancel := context.WithTimeout(ctx, 5*time.Second)
				defer acancel()
				if err := link.client.Answer(actx, q.ID, a); err != nil {
					m.log.Debug("answer to the egress proxy", "workspace", workspace, "err", err)
				}
			})
		}
		for id, r := range active {
			if !open[id] {
				r.cancel()
				delete(active, id)
			}
		}
	}
}

// reloadLoop sends changed policies (sandbox.yaml, secrets) to running proxies.
func (m *Manager) reloadLoop() {
	t := time.NewTicker(m.opts.ReloadInterval)
	defer t.Stop()
	for {
		select {
		case <-m.bg.Done():
			return
		case <-t.C:
		}
		m.ReloadPolicies(m.bg)
	}
}

// ReloadPolicies compares the policy of every connected proxy with the current
// configuration and sends changes (hot reload, without restarting the containers).
func (m *Manager) ReloadPolicies(ctx context.Context) {
	m.mu.Lock()
	src := m.source
	links := make(map[string]*proxyLink, len(m.links))
	for k, v := range m.links {
		links[k] = v
	}
	m.mu.Unlock()
	if src == nil {
		return
	}
	for ws, link := range links {
		spec, err := src(ctx, ws)
		if err != nil {
			// e.g. invalid sandbox.yaml: the most recently sent policy stays.
			m.mu.Lock()
			warn := link.reloadErr != err.Error()
			link.reloadErr = err.Error()
			m.mu.Unlock()
			if warn {
				m.log.Warn("network policy not updated, the previous one stays", "workspace", ws, "err", err)
			}
			continue
		}
		pol := m.policyFor(spec)
		h := policyHash(pol)
		m.mu.Lock()
		same := link.hash == h
		m.mu.Unlock()
		if same {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = link.client.SetPolicy(cctx, pol)
		cancel()
		if err != nil {
			m.log.Warn("policy change not sent to the egress proxy", "workspace", ws, "err", err)
			continue
		}
		m.mu.Lock()
		link.hash, link.reloadErr = h, ""
		m.mu.Unlock()
		m.log.Info("network policy updated", "workspace", ws, "allow", len(pol.Allow), "deny", len(pol.Deny))
	}
}

// SpecSource returns the current description of a workspace (for the hot reload).
type SpecSource func(ctx context.Context, workspace string) (Spec, error)

// SetSpecSource sets the source for the hot reload of the policies.
func (m *Manager) SetSpecSource(src SpecSource) {
	m.mu.Lock()
	m.source = src
	m.mu.Unlock()
}

// SecretProviders names the providers for which the proxy inserts a key (names only).
func (m *Manager) SecretProviders() []string {
	out := []string{}
	if m.opts.Secrets != nil {
		for k := range m.opts.Secrets() {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// SetEgressSink sets the receiver of the egress events.
func (m *Manager) SetEgressSink(fn func(workspace string, events []proxy.Event)) {
	m.mu.Lock()
	m.opts.OnEgress = fn
	m.mu.Unlock()
}

// HookAvailable reports whether agent containers get the ade binary for the approval hook.
func (m *Manager) HookAvailable() bool { return m.opts.ProxyBinary != "" }

// IDEHostPort returns the port on 127.0.0.1 at which a workspace's IDE is reachable
// (through the proxy's forwarding).
func (m *Manager) IDEHostPort(ctx context.Context, workspace string) (int, error) {
	return m.hostPort(ctx, ProxyName(workspace), proxy.IDEPort)
}

type networkInfo struct {
	Name      string
	Workspace string
	DataDir   string
}

// listNetworks reads ADE's networks via inspect (JSON), because the formats of network ls
// differ between Docker and Podman.
func (m *Manager) listNetworks(ctx context.Context) ([]networkInfo, error) {
	out, err := m.run(ctx, nil, "network", "ls", "--quiet", "--filter", "label="+LabelManaged+"=true")
	if err != nil {
		return nil, err
	}
	refs := strings.Fields(string(out))
	if len(refs) == 0 {
		return nil, nil
	}
	out, err = m.run(ctx, nil, append([]string{"network", "inspect", "--"}, refs...)...)
	if err != nil && !isNoSuch(err) {
		return nil, err
	}
	// Docker: Name, Labels; Podman: name, labels (encoding/json ignores the case).
	var raw []struct {
		Name   string
		Labels map[string]string
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		if len(bytes.TrimSpace(out)) == 0 {
			return nil, nil // all disappeared between ls and inspect
		}
		return nil, fmt.Errorf("read network inspect: %w", err)
	}
	nets := make([]networkInfo, 0, len(raw))
	for _, r := range raw {
		if r.Name != "" {
			nets = append(nets, networkInfo{Name: r.Name, Workspace: r.Labels[LabelWorkspace], DataDir: r.Labels[LabelDataDir]})
		}
	}
	return nets, nil
}

// orphanNetworks returns internal networks without a workspace (like Orphans for containers).
// The shared egress network carries no instance and stays.
func (m *Manager) orphanNetworks(ctx context.Context, known func(string) bool) ([]string, error) {
	nets, err := m.listNetworks(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nets {
		switch n.DataDir {
		case m.opts.DataDir:
			if n.Workspace == "" || !known(n.Workspace) {
				out = append(out, n.Name)
			}
		case "":
		default:
			if _, err := os.Stat(n.DataDir); errors.Is(err, os.ErrNotExist) {
				out = append(out, n.Name)
			}
		}
	}
	return out, nil
}

func (m *Manager) removeNetworks(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	_, err := m.run(ctx, nil, append([]string{"network", "rm", "--"}, names...)...)
	if err != nil && isNoSuch(err) {
		return nil
	}
	return err
}
