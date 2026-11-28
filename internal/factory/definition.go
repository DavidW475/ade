// Package factory runs software factories (doc/factory.md): load and check definitions from
// .ade/factories/<name>.yaml, advance runs as a state machine in SQLite, start roles as
// normal agent sessions, create gates as approvals and create a pull request at the
// end.
package factory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Dir is the directory of the factories in the repository.
const Dir = ".ade/factories"

// Limits for definitions.
const (
	maxFileSize   = 256 << 10
	maxPromptSize = 64 << 10
	maxSchemaSize = 64 << 10
	maxSteps      = 50
	// MaxBudgetUSD is the largest allowed budget value.
	MaxBudgetUSD = 1e6
)

// Kinds of steps.
const (
	KindRole   = "role"
	KindGate   = "gate"
	KindAction = "action"
	// KindDecision jumps back if when is true (goto, max_iterations).
	KindDecision = "decision"
)

// Limits and defaults for loops.
const (
	DefaultMaxIterations = 3
	MaxIterations        = 20
)

// Behavior of a decision when max_iterations is reached.
const (
	ExhaustedFail     = "fail"
	ExhaustedContinue = "continue"
)

// Permissions of a role.
const (
	PermRead  = "read"
	PermWrite = "write"
)

// ActionPullRequest creates a pull request at the end (delivery from M4).
const ActionPullRequest = "pull_request"

// GateHuman waits for an approval in the dashboard.
const GateHuman = "human"

// Adapters are the adapters a role may name (structured agents, no pty).
var Adapters = []string{"claude", "codex"}

// Factory is a factory definition.
type Factory struct {
	Name        string           `yaml:"name" json:"name"`
	Description string           `yaml:"description" json:"description,omitempty"`
	Inputs      map[string]Input `yaml:"inputs" json:"inputs,omitempty"`
	Roles       map[string]*Role `yaml:"roles" json:"roles"`
	Flow        []Step           `yaml:"flow" json:"flow"`
	Budget      *Budget          `yaml:"budget" json:"budget,omitempty"`
}

// Kinds of inputs.
const (
	InputText = "text"
	// InputBranch is a branch of the repository; the run's workspace starts from it
	// (default: the project's base branch). At most one per factory.
	InputBranch = "branch"
)

// Input is an input of a run.
type Input struct {
	Type        string `yaml:"type" json:"type"`
	Required    bool   `yaml:"required" json:"required,omitempty"`
	Description string `yaml:"description" json:"description,omitempty"`
}

// Role is a role: an agent with model, permissions and prompt.
type Role struct {
	Adapter string `yaml:"adapter" json:"adapter,omitempty"`
	// Model is an alias (models in config.yaml) or a model ID.
	Model string `yaml:"model" json:"model,omitempty"`
	// Permissions: read (workspace read-only) or write (default).
	Permissions    string `yaml:"permissions" json:"permissions"`
	PermissionMode string `yaml:"permission_mode" json:"permission_mode,omitempty"`
	// Prompt is a file relative to the factory (e.g. prompts/planner.md).
	Prompt string `yaml:"prompt" json:"prompt_file,omitempty"`
	// PromptText is its content (read when loading).
	PromptText string   `yaml:"-" json:"prompt,omitempty"`
	Inputs     []string `yaml:"inputs" json:"inputs,omitempty"`
	// Outputs are the role's artifacts: at most one, the last answer.
	Outputs []Output `yaml:"outputs" json:"outputs,omitempty"`
	// Sandbox only exists for the whole run (one workspace); here only for a clear error
	// message.
	Sandbox any `yaml:"sandbox" json:"-"`
}

// ReadOnly reports whether the role may only read the workspace.
func (r *Role) ReadOnly() bool { return r.Permissions == PermRead }

// Output returns the role's artifact (empty: none).
func (r *Role) Output() string {
	if len(r.Outputs) == 0 {
		return ""
	}
	return r.Outputs[0].Name
}

// OutputSpec returns the role's artifact with schema (nil: none).
func (r *Role) OutputSpec() *Output {
	if len(r.Outputs) == 0 {
		return nil
	}
	return &r.Outputs[0]
}

// Output is an artifact of a role. In YAML a file name or {name, schema}.
type Output struct {
	Name string `yaml:"name" json:"name"`
	// Schema is a file relative to the factory (JSON Schema, subset: Schema).
	Schema string `yaml:"schema" json:"schema_file,omitempty"`
	// SchemaDoc is its content as JSON (read when loading).
	SchemaDoc json.RawMessage `yaml:"-" json:"schema,omitempty"`
}

// Structured reports whether the artifact is JSON (extension .json or schema).
func (o *Output) Structured() bool {
	return o != nil && (strings.HasSuffix(o.Name, ".json") || o.Schema != "")
}

// UnmarshalYAML takes a file name or an object {name, schema}.
func (o *Output) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*o = Output{Name: n.Value}
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: an artifact is a file name or {name, schema}", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "name", "schema":
		default:
			return fmt.Errorf("line %d: field %s does not exist for an artifact (allowed: name, schema)", n.Content[i].Line, k)
		}
	}
	type plain Output
	return n.Decode((*plain)(o))
}

// UnmarshalJSON also accepts the form before M10 (only the name).
func (o *Output) UnmarshalJSON(b []byte) error {
	var name string
	if json.Unmarshal(b, &name) == nil {
		*o = Output{Name: name}
		return nil
	}
	type plain Output
	return json.Unmarshal(b, (*plain)(o))
}

// Step is a step in the flow.
type Step struct {
	Step   string `yaml:"step" json:"step"`
	Role   string `yaml:"role" json:"role,omitempty"`
	Gate   string `yaml:"gate" json:"gate,omitempty"`
	Action string `yaml:"action" json:"action,omitempty"`
	// When is a condition (Expr): false → step skipped; for a decision true → jump back to
	// Goto.
	When string `yaml:"when" json:"when,omitempty"`
	Goto string `yaml:"goto" json:"goto,omitempty"`
	// MaxIterations limits the passes of a loop (decision or gate with on_deny;
	// 0: DefaultMaxIterations).
	MaxIterations int `yaml:"max_iterations" json:"max_iterations,omitempty"`
	// OnExhausted: fail (default) or continue, if a decision would still jump back after
	// max_iterations passes.
	OnExhausted string `yaml:"on_exhausted" json:"on_exhausted,omitempty"`
	// OnDeny makes a gate jump back on denial instead of canceling the run.
	OnDeny *OnDeny `yaml:"on_deny" json:"on_deny,omitempty"`
	// Parallel are role steps that run at the same time (group without a name of its own).
	Parallel []Step `yaml:"parallel" json:"parallel,omitempty"`
	// Unattended allows pull_request without a preceding gate (#103). If the diff changes
	// sensitive paths (SensitivePaths), the run still waits for an approval.
	Unattended bool `yaml:"unattended" json:"unattended,omitempty"`
}

// OnDeny is a gate's jump back on denial.
type OnDeny struct {
	Goto string `yaml:"goto" json:"goto"`
}

// Kind returns the kind of the step.
func (s *Step) Kind() string {
	switch {
	case s.Role != "":
		return KindRole
	case s.Gate != "":
		return KindGate
	case s.Action != "":
		return KindAction
	case s.Goto != "":
		return KindDecision
	}
	return ""
}

// Iterations returns the allowed passes of the step's loop.
func (s *Step) Iterations() int {
	if s.MaxIterations > 0 {
		return s.MaxIterations
	}
	return DefaultMaxIterations
}

// Target returns the target of a jump back (decision: goto, gate: on_deny.goto).
func (s *Step) Target() string {
	if s.OnDeny != nil {
		return s.OnDeny.Goto
	}
	return s.Goto
}

// Pos returns the position of the step name in the flat flow (-1: does not exist).
func (f *Factory) Pos(name string) int {
	for _, s := range f.Flat() {
		if s.Step.Step == name {
			return s.Pos
		}
	}
	return -1
}

// Budget limits a run.
type Budget struct {
	MaxCostUSD float64 `yaml:"max_cost_usd" json:"max_cost_usd,omitempty"`
	// MaxDuration is the agents' working time (waiting at gates does not count).
	MaxDuration time.Duration `yaml:"max_duration" json:"max_duration_ns,omitempty"`
}

// Error is an invalid factory.
type Error struct {
	// Path is the file (e.g. .ade/factories/feature.yaml).
	Path string
	Msg  string
}

func (e *Error) Error() string { return e.Path + ": " + e.Msg }

var (
	nameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	artifactRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
)

// ValidName reports whether name is a valid name for a factory, step or role.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Parse reads a factory from data. path is the path for error messages; read reads a
// file (prompt, schema) relative to the factory (nil: files are not read).
func Parse(path string, data []byte, read func(rel string) ([]byte, error)) (*Factory, error) {
	fail := func(format string, args ...any) error { return &Error{Path: path, Msg: fmt.Sprintf(format, args...)} }
	if len(data) > maxFileSize {
		return nil, fail("file is too large (at most %d KiB)", maxFileSize>>10)
	}
	var f Factory
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fail("file is empty")
		}
		return nil, fail("%s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if msg := f.validate(); msg != "" {
		return nil, fail("%s", msg)
	}
	if read != nil {
		for _, name := range f.roleNames() {
			r := f.Roles[name]
			if err := r.loadSchemas(name, read); err != nil {
				return nil, fail("%v", err)
			}
			if r.Prompt == "" {
				continue
			}
			b, err := read(r.Prompt)
			if err != nil {
				return nil, fail("roles.%s.prompt: %v", name, err)
			}
			if len(b) > maxPromptSize {
				return nil, fail("roles.%s.prompt: file is too large (at most %d KiB)", name, maxPromptSize>>10)
			}
			r.PromptText = strings.TrimSpace(string(b))
		}
		// Check fields in conditions only now against the read schemas.
		for _, st := range f.Flat() {
			if st.When == "" {
				continue
			}
			if msg := f.checkWhen(st, st.When); msg != "" {
				return nil, fail("flow (%s).when: %s", st.Step.Step, msg)
			}
		}
	}
	return &f, nil
}

// loadSchemas reads and checks the schemas of a role's artifacts.
func (r *Role) loadSchemas(name string, read func(rel string) ([]byte, error)) error {
	for i := range r.Outputs {
		o := &r.Outputs[i]
		if o.Schema == "" {
			continue
		}
		at := fmt.Sprintf("roles.%s.outputs.%s.schema", name, o.Name)
		b, err := read(o.Schema)
		if err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		if len(b) > maxSchemaSize {
			return fmt.Errorf("%s: file is too large (at most %d KiB)", at, maxSchemaSize>>10)
		}
		doc, err := schemaJSON(b)
		if err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		if _, err := CompileSchema(doc); err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		o.SchemaDoc = doc
	}
	return nil
}

// schemaJSON reads a schema in JSON or YAML and returns it as JSON.
func schemaJSON(b []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("neither JSON nor YAML: %s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("cannot be represented as JSON: %w", err)
	}
	return out, nil
}

func (f *Factory) roleNames() []string {
	names := make([]string, 0, len(f.Roles))
	for n := range f.Roles {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func (f *Factory) validate() string {
	f.Name = strings.TrimSpace(f.Name)
	if !ValidName(f.Name) {
		return fmt.Sprintf("name: %q is invalid (lowercase letters, digits, - and _)", f.Name)
	}
	if len(f.Description) > 500 {
		return "description: at most 500 characters"
	}
	for _, name := range sortedKeys(f.Inputs) {
		in := f.Inputs[name]
		if !ValidName(name) {
			return fmt.Sprintf("inputs.%s: invalid name", name)
		}
		if in.Type == "" {
			in.Type = InputText
			f.Inputs[name] = in
		}
		switch in.Type {
		case InputText:
		case InputBranch:
			if b := f.BranchInput(); b != name {
				return fmt.Sprintf("inputs.%s.type: at most one input of type branch (already %s)", name, b)
			}
		default:
			return fmt.Sprintf("inputs.%s.type: %q is not supported (allowed: text, branch)", name, in.Type)
		}
	}
	if len(f.Roles) == 0 {
		return "roles: at least one role required"
	}
	outputs := map[string]string{} // artifact → role
	for _, name := range f.roleNames() {
		r := f.Roles[name]
		at := "roles." + name
		if r == nil {
			return at + ": empty"
		}
		if !ValidName(name) {
			return at + ": invalid name (lowercase letters, digits, - and _)"
		}
		if r.Sandbox != nil {
			return at + ".sandbox: applies to the whole run (all roles work in the same workspace) and is chosen at start"
		}
		if r.Adapter == "" {
			r.Adapter = "claude"
		}
		if !slices.Contains(Adapters, r.Adapter) {
			return fmt.Sprintf("%s.adapter: %q is unknown (allowed: %s)", at, r.Adapter, strings.Join(Adapters, ", "))
		}
		if r.Permissions == "" {
			r.Permissions = PermWrite
		}
		if r.Permissions != PermRead && r.Permissions != PermWrite {
			return fmt.Sprintf("%s.permissions: %q is invalid (allowed: read, write)", at, r.Permissions)
		}
		if strings.ContainsAny(r.Model, " \n\x00") || len(r.Model) > 200 {
			return at + ".model: invalid"
		}
		if r.Prompt != "" {
			if msg := checkRel(r.Prompt); msg != "" {
				return at + ".prompt: " + msg
			}
		}
		if len(r.Outputs) > 1 {
			return at + ".outputs: at most one artifact per role (the role's last answer)"
		}
		for _, o := range r.Outputs {
			if !artifactRE.MatchString(o.Name) {
				return fmt.Sprintf("%s.outputs: %q is not a valid file name", at, o.Name)
			}
			if other, ok := outputs[o.Name]; ok {
				return fmt.Sprintf("%s.outputs: %s is already produced by role %s", at, o.Name, other)
			}
			if o.Schema != "" {
				if !strings.HasSuffix(o.Name, ".json") {
					return fmt.Sprintf("%s.outputs: %s has a schema and must end in .json", at, o.Name)
				}
				if msg := checkRel(o.Schema); msg != "" {
					return fmt.Sprintf("%s.outputs.%s.schema: %s", at, o.Name, msg)
				}
			}
			outputs[o.Name] = name
		}
		for _, in := range r.Inputs {
			if !artifactRE.MatchString(in) {
				return fmt.Sprintf("%s.inputs: %q is not a valid file name", at, in)
			}
		}
	}
	for _, in := range f.roleNames() {
		for _, a := range f.Roles[in].Inputs {
			if _, ok := outputs[a]; !ok {
				return fmt.Sprintf("roles.%s.inputs: no role produces %s", in, a)
			}
		}
	}

	if msg := f.validateFlow(); msg != "" {
		return msg
	}
	if b := f.Budget; b != nil {
		if b.MaxCostUSD < 0 || b.MaxCostUSD > MaxBudgetUSD || b.MaxCostUSD != b.MaxCostUSD {
			return fmt.Sprintf("budget.max_cost_usd: between 0 and %.0f", MaxBudgetUSD)
		}
		if b.MaxDuration < 0 || b.MaxDuration > 30*24*time.Hour {
			return "budget.max_duration: between 0 and 720h"
		}
	}
	return ""
}

// Limits for parallel groups.
const (
	minParallel = 2
	maxParallel = 8
)

// FlatStep is a step in the flat flow: members of parallel groups follow each other, Group
// is the position of the first member (-1: no group).
type FlatStep struct {
	*Step
	Pos   int
	Group int
}

// Start returns the position from which the step runs (group start or own position).
func (s FlatStep) Start() int {
	if s.Group >= 0 {
		return s.Group
	}
	return s.Pos
}

// Flat returns the flow flattened (this is how a run creates it).
func (f *Factory) Flat() []FlatStep {
	var out []FlatStep
	for i := range f.Flow {
		s := &f.Flow[i]
		if len(s.Parallel) == 0 {
			out = append(out, FlatStep{Step: s, Pos: len(out), Group: -1})
			continue
		}
		g := len(out)
		for j := range s.Parallel {
			out = append(out, FlatStep{Step: &s.Parallel[j], Pos: len(out), Group: g})
		}
	}
	return out
}

// validateFlow checks the flow: steps, groups, conditions, jumps back and that every role
// gets its inputs (in the first pass or after a jump back).
func (f *Factory) validateFlow() string {
	if len(f.Flow) == 0 {
		return "flow: at least one step required"
	}
	for i := range f.Flow {
		s := &f.Flow[i]
		if len(s.Parallel) == 0 {
			continue
		}
		at := fmt.Sprintf("flow[%d].parallel", i)
		if s.Step != "" || s.Role != "" || s.Gate != "" || s.Action != "" || s.Goto != "" || s.When != "" ||
			s.OnDeny != nil || s.MaxIterations != 0 || s.OnExhausted != "" {
			return at + ": a parallel group only has parallel (conditions go on the individual steps)"
		}
		if len(s.Parallel) < minParallel || len(s.Parallel) > maxParallel {
			return fmt.Sprintf("%s: %d to %d steps", at, minParallel, maxParallel)
		}
		writers := 0
		for j := range s.Parallel {
			m := &s.Parallel[j]
			mat := fmt.Sprintf("%s[%d]", at, j)
			if len(m.Parallel) > 0 {
				return mat + ": parallel groups cannot be nested"
			}
			if m.Role == "" || m.Gate != "" || m.Action != "" || m.Goto != "" || m.OnDeny != nil || m.MaxIterations != 0 || m.OnExhausted != "" {
				return mat + ": only roles run in parallel (role, optionally when)"
			}
			if r := f.Roles[m.Role]; r != nil && !r.ReadOnly() {
				writers++
			}
		}
		if writers > 1 {
			return at + ": at most one role with write permission per group (all work in the same workspace)"
		}
	}
	flat := f.Flat()
	if len(flat) > maxSteps {
		return fmt.Sprintf("flow: at most %d steps", maxSteps)
	}
	pos := map[string]int{}
	label := func(s FlatStep) string { return fmt.Sprintf("flow (%s)", s.Step.Step) }
	for _, s := range flat {
		if !ValidName(s.Step.Step) {
			return fmt.Sprintf("flow: step %q is invalid (lowercase letters, digits, - and _)", s.Step.Step)
		}
		if _, ok := pos[s.Step.Step]; ok {
			return fmt.Sprintf("flow: step %s already exists", s.Step.Step)
		}
		pos[s.Step.Step] = s.Pos
	}
	// Jumps back: origin → target (for inputs that only a later step produces).
	type loop struct{ from, to int }
	var loops []loop
	for _, fs := range flat {
		s := fs.Step
		at := label(fs)
		n := 0
		for _, v := range []string{s.Role, s.Gate, s.Action} {
			if v != "" {
				n++
			}
		}
		if n > 1 || n == 1 && s.Goto != "" {
			return at + ": specify exactly one of role, gate, action or goto"
		}
		if n == 0 && s.Goto == "" {
			if s.When != "" {
				return at + ": a decision needs goto (jump back if when is true)"
			}
			return at + ": specify exactly one of role, gate, action or goto"
		}
		kind := s.Kind()
		if s.When != "" {
			if msg := f.checkWhen(fs, s.When); msg != "" {
				return at + ".when: " + msg
			}
		} else if kind == KindDecision {
			return at + ".when: a decision needs a condition"
		}
		if s.OnDeny != nil && kind != KindGate {
			return at + ".on_deny: only exists for gates"
		}
		if s.Unattended && kind != KindAction {
			return at + ".unattended: only exists for action: pull_request"
		}
		if s.OnExhausted != "" && kind != KindDecision {
			return at + ".on_exhausted: only exists for decisions (goto)"
		}
		if s.MaxIterations != 0 && kind != KindDecision && s.OnDeny == nil {
			return at + ".max_iterations: only exists for decisions (goto) and gates with on_deny"
		}
		if s.MaxIterations < 0 || s.MaxIterations > MaxIterations {
			return fmt.Sprintf("%s.max_iterations: between 1 and %d", at, MaxIterations)
		}
		if t := s.Target(); t != "" {
			field := "goto"
			if s.OnDeny != nil {
				field = "on_deny.goto"
			}
			p, ok := pos[t]
			if !ok {
				return fmt.Sprintf("%s.%s: step %s does not exist", at, field, t)
			}
			to := flat[p].Start()
			if to >= fs.Pos {
				return fmt.Sprintf("%s.%s: only jumps back to earlier steps (%s is not before it)", at, field, t)
			}
			loops = append(loops, loop{from: fs.Pos, to: to})
		}
		switch s.OnExhausted {
		case "", ExhaustedFail, ExhaustedContinue:
		default:
			return fmt.Sprintf("%s.on_exhausted: %q is invalid (allowed: fail, continue)", at, s.OnExhausted)
		}
		switch kind {
		case KindRole:
			if f.Roles[s.Role] == nil {
				return fmt.Sprintf("%s.role: role %s is not defined", at, s.Role)
			}
		case KindGate:
			if s.Gate != GateHuman {
				return fmt.Sprintf("%s.gate: %q is invalid (allowed: human)", at, s.Gate)
			}
		case KindAction:
			if s.Action != ActionPullRequest {
				return fmt.Sprintf("%s.action: %q is invalid (allowed: pull_request)", at, s.Action)
			}
			if fs.Pos != len(flat)-1 {
				return at + ".action: pull_request must be the last step"
			}
			if !s.Unattended && !reviewedBefore(f, flat, fs.Pos) {
				return at + ".action: pull_request needs a gate before it (gate: human, without when) and no role with write permission after it – " +
					"otherwise the run pushes unreviewed agent code; without a gate only with unattended: true"
			}
		}
	}
	// Inputs: in the first pass produced by an earlier step (not from the same group) or
	// reachable through a jump back from behind the producer.
	producers := map[string][]int{}
	for _, s := range flat {
		if r := f.Roles[s.Role]; r != nil {
			for _, o := range r.Outputs {
				producers[o.Name] = append(producers[o.Name], s.Pos)
			}
		}
	}
	for _, s := range flat {
		r := f.Roles[s.Role]
		if r == nil {
			continue
		}
		start := s.Start()
		for _, a := range r.Inputs {
			ok := false
			for _, p := range producers[a] {
				if p < start {
					ok = true
				}
				for _, l := range loops {
					if l.from >= p && l.to <= start {
						ok = true
					}
				}
			}
			if !ok {
				return fmt.Sprintf("%s: role %s needs %s, which only a later step produces (and no jump back leads there)", label(s), s.Role, a)
			}
		}
	}
	return ""
}

// reviewedBefore reports whether a gate without a condition lies before position pos, after
// which no role with write permission runs: then a human has seen the state that is pushed.
// Jumps back behind the gate do not change that, they lead through the gate again.
func reviewedBefore(f *Factory, flat []FlatStep, pos int) bool {
	gate := -1
	for _, s := range flat[:pos] {
		if s.Kind() == KindGate && s.When == "" {
			gate = s.Pos
		}
	}
	if gate < 0 {
		return false
	}
	for _, s := range flat[gate+1 : pos] {
		if r := f.Roles[s.Role]; r != nil && !r.ReadOnly() {
			return false
		}
	}
	return true
}

// checkWhen checks a step's condition: every path names an earlier step (not from the same
// group) with a structured artifact and, if the schema names fields, one of them.
func (f *Factory) checkWhen(at FlatStep, src string) string {
	e, err := ParseExpr(src)
	if err != nil {
		return err.Error()
	}
	flat := f.Flat()
	for _, path := range e.Paths() {
		p := f.Pos(path[0])
		if p < 0 {
			return fmt.Sprintf("step %s does not exist", path[0])
		}
		if p >= at.Start() {
			return fmt.Sprintf("%s is not an earlier step", path[0])
		}
		var out *Output
		if r := f.Roles[flat[p].Role]; r != nil {
			out = r.OutputSpec()
		}
		if !out.Structured() {
			return fmt.Sprintf("step %s has no structured artifact (outputs with .json)", path[0])
		}
		if len(out.SchemaDoc) > 0 {
			if sch, err := CompileSchema(out.SchemaDoc); err == nil && len(sch.Properties) > 0 && sch.Properties[path[1]] == nil {
				return fmt.Sprintf("according to its schema, %s has no field %s", out.Name, path[1])
			}
		}
	}
	return ""
}

// checkRel checks a path relative to the factory file: no absolute path, no “..”.
func checkRel(p string) string {
	if strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return fmt.Sprintf("%q must be a relative path", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Sprintf("%q must not lead out of %s", p, Dir)
		}
	}
	return ""
}

// BranchInput returns the name of the input of type branch (empty: none).
func (f *Factory) BranchInput() string {
	for _, k := range sortedKeys(f.Inputs) {
		if f.Inputs[k].Type == InputBranch {
			return k
		}
	}
	return ""
}

// CheckInputs checks the inputs of a run and returns them cleaned up.
func (f *Factory) CheckInputs(in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range in {
		if _, ok := f.Inputs[k]; !ok {
			return nil, fmt.Errorf("input %s does not exist in factory %s", k, f.Name)
		}
		v = strings.TrimSpace(v)
		if len(v) > 100<<10 {
			return nil, fmt.Errorf("input %s is too long (at most 100 KiB)", k)
		}
		if v != "" {
			out[k] = v
		}
	}
	for _, k := range sortedKeys(f.Inputs) {
		if f.Inputs[k].Required && out[k] == "" {
			return nil, fmt.Errorf("input %s missing", k)
		}
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
