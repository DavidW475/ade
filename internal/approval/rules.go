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

// Config is the approvals section in .ade/project.yaml: which tools run without asking,
// separately for agents in the sandbox and locally. If a part is missing, its default
// applies (DefaultSandbox or DefaultLocal).
type Config struct {
	Sandbox *RuleSet `yaml:"sandbox" json:"sandbox,omitempty"`
	Local   *RuleSet `yaml:"local" json:"local,omitempty"`
}

// RuleSet are rules for one environment. Entries: "Tool" or "Tool(pattern)", e.g.
// "*" (any tool), "Bash(go test *)" (command that starts like this; a trailing " *" also
// matches without arguments), "Edit(**)" (file in the workspace; * within a directory, **
// across directories), "Read(/etc/**)" (absolute path, also ~/…),
// "WebFetch(*.example.com)" (host of the URL), "mcp__*" (tool names with a wildcard).
//
// Order: deny before ask before allow; anything that matches nowhere needs an approval.
type RuleSet struct {
	Allow []string `yaml:"allow" json:"allow"`
	Ask   []string `yaml:"ask" json:"ask"`
	Deny  []string `yaml:"deny" json:"deny"`
}

// DefaultSandbox applies without approvals.sandbox: in the sandbox the container limits the damage – everything allowed.
var DefaultSandbox = RuleSet{Allow: []string{"*"}}

// DefaultLocal applies without approvals.local: without a sandbox only reading and editing files in the workspace and
// the CLI's internal tools; commands, web access and MCP tools need an approval.
var DefaultLocal = RuleSet{Allow: []string{
	"Read(**)", "Glob(**)", "Grep(**)", "LS(**)", "NotebookRead(**)",
	"Edit(**)", "MultiEdit(**)", "Write(**)", "NotebookEdit(**)",
	"TodoWrite", "Task", "Agent", "ExitPlanMode", "BashOutput",
}}

// Effective returns the effective rules of an environment.
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

// Validate checks all entries.
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

// Verdicts of a rule check.
const (
	VerdictAllow = "allow"
	VerdictAsk   = "ask"
	VerdictDeny  = "deny"
)

// Rules are checked rules.
type Rules struct {
	allow, ask, deny []rule
}

type rule struct {
	raw  string
	tool *regexp.Regexp
	// arg is nil without parentheses (then any input matches).
	arg     string
	argRE   *regexp.Regexp
	absPath bool
}

var ruleRE = regexp.MustCompile(`^([A-Za-z0-9_*.:-]+)(?:\((.*)\))?$`)

// Compile checks a RuleSet.
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
		return rule{}, fmt.Errorf("%q is not a valid rule (Tool or Tool(pattern))", raw)
	}
	r := rule{raw: s, tool: globRE(m[1], false)}
	if strings.Contains(s, "(") {
		r.arg = strings.TrimSpace(m[2])
		if r.arg == "" {
			return rule{}, fmt.Errorf("%q: empty pattern", raw)
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

// globRE translates a pattern: * (for paths without /), ** (across directories), ?.
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

// pathFields are the input fields with paths per Claude Code tool.
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

// Call is a tool call as the rules see it.
type Call struct {
	Tool  string
	Input json.RawMessage
	// Workdir is the workspace directory from the agent's point of view (locally the path on
	// the host, in the sandbox /workspace).
	Workdir string
	// ResolveLinks: resolve symlinks on the host (only locally; in the sandbox the daemon
	// does not see the container's file system).
	ResolveLinks bool
}

// Evaluate checks a call: deny before ask before allow, otherwise ask. Returns the verdict
// and the matching rule (empty: none).
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

// callArg is the argument of a call for the patterns.
type callArg struct {
	kind argKind
	// rel is the path relative to the workspace ("" outside), abs the absolute one.
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

// resolve resolves symlinks in the longest existing part of the path.
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

// compound detects compound shell commands (chaining, pipes, redirections,
// substitutions). Allow rules never match them.
var compound = regexp.MustCompile("[;&|<>`\\n\\r]|\\$\\(")

// segments roughly splits a command at separators (for deny/ask).
var segmentSplit = regexp.MustCompile("[;&|\\n\\r]+|\\$\\(|`|\\)")

func matchAny(rules []rule, tool string, a callArg, strict bool) string {
	for _, r := range rules {
		if r.match(tool, a, strict) {
			return r.raw
		}
	}
	return ""
}

// match checks a rule. strict (deny/ask): for commands a sub-command matches as well.
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
			return false // outside the workspace no relative pattern matches
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
