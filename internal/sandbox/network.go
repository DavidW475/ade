package sandbox

// Netz und Egress-Proxy eines Workspaces (doc/sandbox.md, Abschnitt 1 und 3):
//
//	ade-ws-<id>    Docker-Netz mit --internal: keine Route nach draußen
//	ade-ws-<id>    Agent-Container, nur in diesem Netz (Alias agent)
//	ade-proxy-<id> Proxy-Container (ade proxy), in ade-egress und ade-ws-<id> (Alias proxy)
//	ade-egress     gemeinsames Netz aller Proxys mit Internet, ohne Verkehr zwischen Containern
//
// Docker veröffentlicht Ports von Containern in internen Netzen nicht. Der Daemon erreicht
// deshalb Steuer-API und IDE über Ports des Proxy-Containers auf 127.0.0.1; der Proxy leitet
// die IDE an agent:3000 weiter.

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

// Namen und Werte des Sandbox-Netzes.
const (
	EgressNetwork = "ade-egress"
	// ProxyAlias ist der Name des Proxys im Workspace-Netz.
	ProxyAlias = "proxy"
	// AgentAlias ist der Name des Agent-Containers im Workspace-Netz.
	AgentAlias = "agent"
	// NetVersion kennzeichnet den Aufbau der Container (proxy-1: Netz aus M6, proxy-2: dazu
	// das ade-Binary für den Freigabe-Hook, M7); ältere werden beim nächsten Start neu angelegt.
	NetVersion = "proxy-2"
	// ProxyBinary ist der Pfad des ade-Binarys im Proxy- und im Agent-Container.
	ProxyBinary = "/opt/ade/ade"
	// noDNS ist der DNS-Server des Agent-Containers für externe Namen: eine Adresse aus
	// TEST-NET-1, die im internen Netz nicht erreichbar ist. Container-Namen (proxy) löst
	// Docker's eingebetteter DNS weiter auf; externe Namen löst nur der Proxy auf (#57).
	noDNS = "192.0.2.1"
)

// ProxyURL ist die Adresse des Proxys aus Sicht des Agent-Containers.
var ProxyURL = "http://" + ProxyAlias + ":" + strconv.Itoa(proxy.Port)

// ProviderBaseURL ist die Basis-URL der Secret-Injection für einen Anbieter.
func ProviderBaseURL(provider string) string { return ProxyURL + "/p/" + provider }

// NetworkName ist der Name des internen Netzes eines Workspaces.
func NetworkName(workspace string) string { return "ade-ws-" + workspace }

// ProxyName ist der Name des Proxy-Containers eines Workspaces.
func ProxyName(workspace string) string { return "ade-proxy-" + workspace }

// proxyEnv sind die Proxy-Variablen im Agent-Container (Bequemlichkeit – erzwungen wird die
// Isolation durch das interne Netz).
func proxyEnv() []string {
	// proxy selbst direkt ansprechen (Basis-URLs der Secret-Injection).
	noProxy := "localhost,127.0.0.1,::1," + ProxyAlias
	return []string{
		"HTTP_PROXY=" + ProxyURL, "HTTPS_PROXY=" + ProxyURL, "http_proxy=" + ProxyURL, "https_proxy=" + ProxyURL,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
	}
}

// ProxyCreateArgs liefert die Argumente für docker create des Proxy-Containers (für Tests
// exportiert). Das Token kommt über die Umgebung der CLI, nicht über die Argumente.
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
	// Gleiche UID-Abbildung wie der Agent-Container: das gemountete Binary gehört dem Nutzer
	// und ist nur für ihn lesbar.
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

// ensureNetworks legt das Egress-Netz und das interne Netz des Workspaces an, falls nötig.
// created meldet ein neu angelegtes Workspace-Netz.
func (m *Manager) ensureNetworks(ctx context.Context, workspace string) (created bool, err error) {
	if _, err := m.run(ctx, nil, "network", "inspect", "--format", "{{.Name}}", EgressNetwork); err != nil {
		if !isNoSuch(err) {
			return false, fmt.Errorf("Egress-Netz prüfen: %w", err)
		}
		args := append([]string{"network", "create", "--driver", "bridge", "--label", LabelManaged + "=true"}, m.egressNetworkOpts()...)
		_, err := m.run(ctx, nil, append(args, EgressNetwork)...)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return false, fmt.Errorf("Egress-Netz anlegen: %w", err)
		}
	}
	out, err := m.run(ctx, nil, "network", "inspect", "--format", `{{.Internal}} {{index .Labels "`+LabelDataDir+`"}}`, NetworkName(workspace))
	if err == nil {
		internal, dataDir, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
		if internal != "true" || dataDir != m.opts.DataDir {
			return false, fmt.Errorf("Netz %s ist nicht das interne Netz dieser ADE-Instanz", NetworkName(workspace))
		}
		return false, nil
	}
	if !isNoSuch(err) {
		return false, fmt.Errorf("Workspace-Netz prüfen: %w", err)
	}
	if _, err := m.run(ctx, nil, "network", "create", "--driver", "bridge", "--internal",
		"--label", LabelManaged+"=true",
		"--label", LabelWorkspace+"="+workspace,
		"--label", LabelDataDir+"="+m.opts.DataDir,
		NetworkName(workspace)); err != nil {
		return false, fmt.Errorf("Workspace-Netz anlegen: %w", err)
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

// getProxy liefert den Proxy-Container eines Workspaces oder nil.
func (m *Manager) getProxy(ctx context.Context, workspace string) (*Container, error) {
	cs, err := m.inspect(ctx, ProxyName(workspace))
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	c := cs[0]
	if c.DataDir != m.opts.DataDir || c.Workspace != workspace || c.Role != RoleProxy {
		return nil, fmt.Errorf("Container %s gehört nicht zu dieser ADE-Instanz", c.Name)
	}
	return &c, nil
}

// ensureProxy legt den Proxy-Container an (falls nötig) und startet ihn.
func (m *Manager) ensureProxy(ctx context.Context, workspace string, recreate bool) (*Container, error) {
	if m.opts.ProxyBinary == "" {
		return nil, errors.New("kein ade-Binary für den Egress-Proxy konfiguriert")
	}
	p, err := m.getProxy(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if p != nil && (recreate || p.Net != NetVersion || p.Binary != m.opts.ProxyBinary) {
		if _, err := m.run(ctx, nil, "container", "rm", "--force", "--", p.ID); err != nil {
			return nil, fmt.Errorf("Proxy-Container ersetzen: %w", err)
		}
		p = nil
	}
	if p == nil {
		if strings.ContainsAny(m.opts.ProxyBinary, ",\n") || !filepath.IsAbs(m.opts.ProxyBinary) {
			return nil, fmt.Errorf("Pfad %q des ade-Binarys ist für einen Mount ungeeignet", m.opts.ProxyBinary)
		}
		token := randomToken()
		cmd := m.command(ctx, m.ProxyCreateArgs(workspace)...)
		cmd.Env = append(cmd.Env, proxy.EnvToken+"="+token)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("Proxy-Container anlegen: %w", &Error{Args: []string{"create"}, Stderr: stderr.String(), Err: err})
		}
		if _, err := m.run(ctx, nil, "network", "connect", "--alias", ProxyAlias, NetworkName(workspace), ProxyName(workspace)); err != nil {
			return nil, fmt.Errorf("Proxy mit dem Workspace-Netz verbinden: %w", err)
		}
		m.log.Info("Egress-Proxy angelegt", "workspace", workspace)
		if p, err = m.getProxy(ctx, workspace); err != nil || p == nil {
			return nil, fmt.Errorf("Proxy-Container nach dem Anlegen nicht gefunden: %w", err)
		}
	}
	if !p.Running {
		if _, err := m.run(ctx, nil, "container", "start", ProxyName(workspace)); err != nil {
			return nil, fmt.Errorf("Proxy-Container starten: %w", err)
		}
	}
	return p, nil
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// proxyLink ist die Verbindung des Daemons zur Steuer-API eines Proxys.
type proxyLink struct {
	client *proxy.Client
	hash   string // zuletzt gesendete Policy
	// reloadErr ist der letzte Fehler beim Hot-Reload (nur einmal melden).
	reloadErr string
	cancel    context.CancelFunc
	done      chan struct{}
}

// policyFor baut die Policy eines Workspaces aus seiner Konfiguration, den Ausnahmen aus
// Freigaben und den Secrets.
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

// connectProxy verbindet sich mit der Steuer-API des Proxys, schickt die Policy und startet
// das Abholen der Events.
func (m *Manager) connectProxy(ctx context.Context, workspace string, pol proxy.Policy) error {
	p, err := m.getProxy(ctx, workspace)
	if err != nil || p == nil {
		return fmt.Errorf("Proxy-Container: %w", err)
	}
	port, err := m.hostPort(ctx, ProxyName(workspace), proxy.ControlPort)
	if err != nil {
		return err
	}
	link := &proxyLink{client: &proxy.Client{Base: "http://127.0.0.1:" + strconv.Itoa(port), Token: p.token, HTTP: m.http}}
	// Der Proxy braucht nach dem Start einen Moment.
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
		return fmt.Errorf("Egress-Proxy antwortet nicht: %w", err)
	}
	if err := link.client.SetPolicy(ctx, pol); err != nil {
		return fmt.Errorf("Policy an den Egress-Proxy senden: %w", err)
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
		cancel() // Proxy weg: auch das Abholen der Fragen beenden
		wg.Wait()
	}()
	return nil
}

// unlink beendet das Abholen der Events eines Workspaces.
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

// collect holt Events per Long-Polling ab, bis ctx endet oder der Proxy dauerhaft weg ist.
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
				m.log.Debug("Egress-Proxy nicht mehr erreichbar", "workspace", workspace, "err", err)
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

// QuestionHandler beantwortet eine Frage eines Proxys (siehe Options.OnQuestion).
type QuestionHandler func(ctx context.Context, workspace string, q proxy.Question) proxy.Answer

// SetQuestionHandler setzt den Handler für Fragen der Proxys.
func (m *Manager) SetQuestionHandler(h QuestionHandler) {
	m.mu.Lock()
	m.opts.OnQuestion = h
	m.mu.Unlock()
}

// answerQuestions holt offene Fragen per Long-Polling ab und beantwortet jede in einer eigenen
// Goroutine. Verschwindet eine Frage beim Proxy (der Fragende hat aufgegeben), endet der
// Kontext ihres Handlers.
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
				a := proxy.Answer{Decision: proxy.AnswerDeny, Reason: "ADE: Freigaben sind nicht eingerichtet"}
				if h != nil {
					a = h(qctx, workspace, q)
				}
				if qctx.Err() != nil {
					return
				}
				actx, acancel := context.WithTimeout(ctx, 5*time.Second)
				defer acancel()
				if err := link.client.Answer(actx, q.ID, a); err != nil {
					m.log.Debug("Antwort an den Egress-Proxy", "workspace", workspace, "err", err)
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

// reloadLoop schickt geänderte Policies (sandbox.yaml, Secrets) an laufende Proxys.
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

// ReloadPolicies vergleicht die Policy jedes verbundenen Proxys mit der aktuellen
// Konfiguration und schickt Änderungen (Hot-Reload, ohne Neustart der Container).
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
			// z. B. ungültige sandbox.yaml: die zuletzt gesendete Policy bleibt.
			m.mu.Lock()
			warn := link.reloadErr != err.Error()
			link.reloadErr = err.Error()
			m.mu.Unlock()
			if warn {
				m.log.Warn("Netzwerk-Policy nicht aktualisiert, die bisherige bleibt", "workspace", ws, "err", err)
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
			m.log.Warn("Policy-Änderung nicht an den Egress-Proxy gesendet", "workspace", ws, "err", err)
			continue
		}
		m.mu.Lock()
		link.hash, link.reloadErr = h, ""
		m.mu.Unlock()
		m.log.Info("Netzwerk-Policy aktualisiert", "workspace", ws, "allow", len(pol.Allow), "deny", len(pol.Deny))
	}
}

// SpecSource liefert die aktuelle Beschreibung eines Workspaces (für den Hot-Reload).
type SpecSource func(ctx context.Context, workspace string) (Spec, error)

// SetSpecSource setzt die Quelle für den Hot-Reload der Policies.
func (m *Manager) SetSpecSource(src SpecSource) {
	m.mu.Lock()
	m.source = src
	m.mu.Unlock()
}

// SecretProviders nennt die Anbieter, für die der Proxy einen Key einfügt (nur Namen).
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

// SetEgressSink setzt den Empfänger der Egress-Events.
func (m *Manager) SetEgressSink(fn func(workspace string, events []proxy.Event)) {
	m.mu.Lock()
	m.opts.OnEgress = fn
	m.mu.Unlock()
}

// HookAvailable meldet, ob Agent-Container das ade-Binary für den Freigabe-Hook bekommen.
func (m *Manager) HookAvailable() bool { return m.opts.ProxyBinary != "" }

// IDEHostPort liefert den Port auf 127.0.0.1, unter dem die IDE eines Workspaces erreichbar
// ist (über die Weiterleitung des Proxys).
func (m *Manager) IDEHostPort(ctx context.Context, workspace string) (int, error) {
	return m.hostPort(ctx, ProxyName(workspace), proxy.IDEPort)
}

type networkInfo struct {
	Name      string
	Workspace string
	DataDir   string
}

// listNetworks liest die Netze von ADE per inspect (JSON), weil sich die Formate von
// network ls zwischen Docker und Podman unterscheiden.
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
	// Docker: Name, Labels; Podman: name, labels (encoding/json ignoriert die Schreibweise).
	var raw []struct {
		Name   string
		Labels map[string]string
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		if len(bytes.TrimSpace(out)) == 0 {
			return nil, nil // alle zwischen ls und inspect verschwunden
		}
		return nil, fmt.Errorf("network inspect lesen: %w", err)
	}
	nets := make([]networkInfo, 0, len(raw))
	for _, r := range raw {
		if r.Name != "" {
			nets = append(nets, networkInfo{Name: r.Name, Workspace: r.Labels[LabelWorkspace], DataDir: r.Labels[LabelDataDir]})
		}
	}
	return nets, nil
}

// orphanNetworks liefert interne Netze ohne Workspace (wie Orphans für Container). Das
// gemeinsame Egress-Netz trägt keine Instanz und bleibt stehen.
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
