package approval

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config ist der Abschnitt approvals in .ade/project.yaml: welche Werkzeuge ohne Rückfrage
// laufen, getrennt für Agenten in der Sandbox und lokal. Fehlt ein Teil, gilt dessen
// Voreinstellung (DefaultSandbox bzw. DefaultLocal).
type Config struct {
	Sandbox *RuleSet `yaml:"sandbox" json:"sandbox,omitempty"`
	Local   *RuleSet `yaml:"local" json:"local,omitempty"`
}

// RuleSet sind Regeln für eine Umgebung. Einträge: "Tool" oder "Tool(muster)", z. B.
// "*" (jedes Werkzeug), "Bash(go test *)" (Befehl, der so beginnt; ein abschließendes " *"
// passt auch ohne Argumente), "Edit(**)" (Datei im Workspace; * innerhalb eines
// Verzeichnisses, ** darüber hinaus), "Read(/etc/**)" (absoluter Pfad, auch ~/…),
// "WebFetch(*.example.com)" (Host der URL), "mcp__*" (Werkzeugnamen mit Platzhalter).
//
// Reihenfolge: deny vor ask vor allow; was nirgends passt, braucht eine Freigabe.
type RuleSet struct {
	Allow []string `yaml:"allow" json:"allow"`
	Ask   []string `yaml:"ask" json:"ask"`
	Deny  []string `yaml:"deny" json:"deny"`
}

// DefaultSandbox gilt ohne approvals.sandbox: In der Sandbox begrenzt der Container den Schaden – alles erlaubt.
var DefaultSandbox = RuleSet{Allow: []string{"*"}}

// DefaultLocal gilt ohne approvals.local: Ohne Sandbox nur Lesen und Ändern von Dateien im Workspace sowie interne
// Werkzeuge der CLI; Befehle, Web-Zugriffe und MCP-Werkzeuge brauchen eine Freigabe.
var DefaultLocal = RuleSet{Allow: []string{
	"Read(**)", "Glob(**)", "Grep(**)", "LS(**)", "NotebookRead(**)",
	"Edit(**)", "MultiEdit(**)", "Write(**)", "NotebookEdit(**)",
	"TodoWrite", "Task", "Agent", "ExitPlanMode", "BashOutput",
}}

// Effective liefert die wirksamen Regeln einer Umgebung.
func (c *Config) Effective(sandbox bool) RuleSet {
	switch {
	case c != nil && sandbox && c.Sandbox != nil:
		return *c.Sandbox
	case c != nil && !sandbox && c.Local != nil:
		return *c.Local
	case sandbox:
		return DefaultSandbox
	}
	return DefaultLocal
}

// Validate prüft alle Einträge.
func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	for name, rs := range map[string]*RuleSet{"sandbox": c.Sandbox, "local": c.Local} {
		if rs == nil {
			continue
		}
		if _, err := Compile(*rs); err != nil {
			return fmt.Errorf("approvals.%s.%w", name, err)
		}
	}
	return nil
}

// Verdikte einer Regelprüfung.
const (
	VerdictAllow = "allow"
	VerdictAsk   = "ask"
	VerdictDeny  = "deny"
)

// Rules sind geprüfte Regeln.
type Rules struct {
	allow, ask, deny []rule
}

type rule struct {
	raw  string
	tool *regexp.Regexp
	// arg ist nil ohne Klammern (dann passt jede Eingabe).
	arg     string
	argRE   *regexp.Regexp
	absPath bool
}

var ruleRE = regexp.MustCompile(`^([A-Za-z0-9_*.:-]+)(?:\((.*)\))?$`)

// Compile prüft ein RuleSet.
func Compile(rs RuleSet) (*Rules, error) {
	out := &Rules{}
	for _, l := range []struct {
		name string
		in   []string
		dst  *[]rule
	}{{"allow", rs.Allow, &out.allow}, {"ask", rs.Ask, &out.ask}, {"deny", rs.Deny, &out.deny}} {
		for _, s := range l.in {
			r, err := parseRule(s)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", l.name, err)
			}
			*l.dst = append(*l.dst, r)
		}
	}
	return out, nil
}

func parseRule(s string) (rule, error) {
	raw := s
	s = strings.TrimSpace(s)
	m := ruleRE.FindStringSubmatch(s)
	if m == nil {
		return rule{}, fmt.Errorf("%q ist keine gültige Regel (Tool oder Tool(muster))", raw)
	}
	r := rule{raw: s, tool: globRE(m[1], false)}
	if strings.Contains(s, "(") {
		r.arg = strings.TrimSpace(m[2])
		if r.arg == "" {
			return rule{}, fmt.Errorf("%q: leeres Muster", raw)
		}
		switch kindOf(m[1]) {
		case argPath:
			p := r.arg
			if strings.HasPrefix(p, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					p = filepath.Join(home, p[2:])
				}
			}
			r.absPath = strings.HasPrefix(p, "/")
			r.argRE = globRE(p, true)
		case argCommand:
			p := r.arg
			if rest, ok := strings.CutSuffix(p, " *"); ok {
				r.argRE = regexp.MustCompile("^" + globBody(rest, false) + "( .*)?$")
			} else {
				r.argRE = globRE(p, false)
			}
		default:
			r.argRE = globRE(r.arg, false)
		}
	}
	return r, nil
}

// globRE übersetzt ein Muster: * (bei Pfaden ohne /), ** (über Verzeichnisse), ?.
func globRE(p string, path bool) *regexp.Regexp {
	return regexp.MustCompile("^" + globBody(p, path) + "$")
}

func globBody(p string, path bool) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			i++
			if path && i+1 < len(p) && p[i+1] == '/' {
				i++
				b.WriteString("(.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			if path {
				b.WriteString("[^/]*")
			} else {
				b.WriteString(".*")
			}
		case c == '?':
			if path {
				b.WriteString("[^/]")
			} else {
				b.WriteString(".")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

type argKind int

const (
	argOther argKind = iota
	argPath
	argCommand
	argURL
)

// pathFields sind die Eingabefelder mit Pfaden je Werkzeug von Claude Code.
var pathFields = map[string]string{
	"Read": "file_path", "Edit": "file_path", "MultiEdit": "file_path", "Write": "file_path",
	"NotebookEdit": "notebook_path", "NotebookRead": "notebook_path",
	"Glob": "path", "Grep": "path", "LS": "path",
}

func kindOf(tool string) argKind {
	switch {
	case pathFields[tool] != "":
		return argPath
	case tool == "Bash":
		return argCommand
	case tool == "WebFetch":
		return argURL
	}
	return argOther
}

// Call ist ein Werkzeug-Aufruf, wie die Regeln ihn sehen.
type Call struct {
	Tool  string
	Input json.RawMessage
	// Workdir ist das Workspace-Verzeichnis aus Sicht des Agenten (lokal der Pfad auf dem
	// Host, in der Sandbox /workspace).
	Workdir string
	// ResolveLinks: Symlinks auf dem Host auflösen (nur lokal; in der Sandbox sieht der
	// Daemon das Dateisystem des Containers nicht).
	ResolveLinks bool
}

// Evaluate prüft einen Aufruf: deny vor ask vor allow, sonst ask. Liefert das Verdikt und
// die passende Regel (leer: keine).
func (rs *Rules) Evaluate(c Call) (string, string) {
	arg := c.arg()
	if r := matchAny(rs.deny, c.Tool, arg, true); r != "" {
		return VerdictDeny, r
	}
	if r := matchAny(rs.ask, c.Tool, arg, true); r != "" {
		return VerdictAsk, r
	}
	if r := matchAny(rs.allow, c.Tool, arg, false); r != "" {
		return VerdictAllow, r
	}
	return VerdictAsk, ""
}

// callArg ist das Argument eines Aufrufs für die Muster.
type callArg struct {
	kind argKind
	// rel ist der Pfad relativ zum Workspace ("" außerhalb), abs der absolute.
	rel, abs string
	cmd      string
	host     string
}

func (c Call) arg() callArg {
	var in map[string]any
	_ = json.Unmarshal(c.Input, &in)
	str := func(k string) string { v, _ := in[k].(string); return v }
	a := callArg{kind: kindOf(c.Tool)}
	switch a.kind {
	case argPath:
		p := str(pathFields[c.Tool])
		if p == "" {
			p = c.Workdir
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.Workdir, p)
		}
		p = filepath.Clean(p)
		wd := filepath.Clean(c.Workdir)
		if c.ResolveLinks {
			p, wd = resolve(p), resolve(wd)
		}
		a.abs = p
		if rel, err := filepath.Rel(wd, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			a.rel = rel
		}
	case argCommand:
		a.cmd = strings.TrimSpace(str("command"))
	case argURL:
		if u, err := url.Parse(str("url")); err == nil {
			a.host = strings.ToLower(u.Hostname())
		}
	}
	return a
}

// resolve löst Symlinks im längsten vorhandenen Teil des Pfads auf.
func resolve(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// compound erkennt zusammengesetzte Shell-Befehle (Verkettung, Pipes, Umleitungen,
// Ersetzungen). Allow-Regeln passen nie auf sie.
var compound = regexp.MustCompile("[;&|<>`\\n\\r]|\\$\\(")

// segments zerlegt einen Befehl grob an Trennzeichen (für deny/ask).
var segmentSplit = regexp.MustCompile("[;&|\\n\\r]+|\\$\\(|`|\\)")

func matchAny(rules []rule, tool string, a callArg, strict bool) string {
	for _, r := range rules {
		if r.match(tool, a, strict) {
			return r.raw
		}
	}
	return ""
}

// match prüft eine Regel. strict (deny/ask): bei Befehlen passt auch ein Teilbefehl.
func (r rule) match(tool string, a callArg, strict bool) bool {
	if !r.tool.MatchString(tool) {
		return false
	}
	if r.argRE == nil {
		return true
	}
	switch a.kind {
	case argPath:
		if r.absPath {
			return r.argRE.MatchString(a.abs)
		}
		if a.rel == "" {
			return false // außerhalb des Workspaces passt kein relatives Muster
		}
		return r.argRE.MatchString(a.rel)
	case argCommand:
		if a.cmd == "" {
			return false
		}
		if !strict {
			return !compound.MatchString(a.cmd) && r.argRE.MatchString(a.cmd)
		}
		if r.argRE.MatchString(a.cmd) {
			return true
		}
		for _, seg := range segmentSplit.Split(a.cmd, -1) {
			if seg = strings.TrimSpace(seg); seg != "" && r.argRE.MatchString(seg) {
				return true
			}
		}
		return false
	case argURL:
		return a.host != "" && r.argRE.MatchString(a.host)
	}
	return false
}
