// Package factory führt Software-Fabriken aus (doc/factory.md): Definitionen aus
// .ade/factories/<name>.yaml laden und prüfen, Runs als Zustandsmaschine in SQLite
// fortschreiben, Rollen als normale Agent-Sessions starten, Gates als Freigaben anlegen und
// am Ende einen Pull Request erstellen.
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

// Dir ist das Verzeichnis der Fabriken im Repo.
const Dir = ".ade/factories"

// Grenzen für Definitionen.
const (
	maxFileSize   = 256 << 10
	maxPromptSize = 64 << 10
	maxSchemaSize = 64 << 10
	maxSteps      = 50
	// MaxBudgetUSD ist die größte erlaubte Budget-Angabe.
	MaxBudgetUSD = 1e6
)

// Arten von Schritten.
const (
	KindRole   = "role"
	KindGate   = "gate"
	KindAction = "action"
	// KindDecision springt zurück, wenn when wahr ist (goto, max_iterations).
	KindDecision = "decision"
)

// Grenzen und Voreinstellungen für Schleifen.
const (
	DefaultMaxIterations = 3
	MaxIterations        = 20
)

// Verhalten einer Entscheidung, wenn max_iterations erreicht ist.
const (
	ExhaustedFail     = "fail"
	ExhaustedContinue = "continue"
)

// Rechte einer Rolle.
const (
	PermRead  = "read"
	PermWrite = "write"
)

// ActionPullRequest erstellt am Ende einen Pull Request (Übernahme aus M4).
const ActionPullRequest = "pull_request"

// GateHuman wartet auf eine Freigabe im Dashboard.
const GateHuman = "human"

// Adapters sind die Adapter, die eine Rolle nennen darf (strukturierte Agenten, kein pty).
var Adapters = []string{"claude", "codex"}

// Factory ist eine Fabrik-Definition.
type Factory struct {
	Name        string           `yaml:"name" json:"name"`
	Description string           `yaml:"description" json:"description,omitempty"`
	Inputs      map[string]Input `yaml:"inputs" json:"inputs,omitempty"`
	Roles       map[string]*Role `yaml:"roles" json:"roles"`
	Flow        []Step           `yaml:"flow" json:"flow"`
	Budget      *Budget          `yaml:"budget" json:"budget,omitempty"`
}

// Arten von Eingaben.
const (
	InputText = "text"
	// InputBranch ist ein Branch des Repos; der Workspace des Runs zweigt von ihm ab
	// (Standard: Basis-Branch des Projekts). Höchstens eine je Fabrik.
	InputBranch = "branch"
)

// Input ist eine Eingabe eines Runs.
type Input struct {
	Type        string `yaml:"type" json:"type"`
	Required    bool   `yaml:"required" json:"required,omitempty"`
	Description string `yaml:"description" json:"description,omitempty"`
}

// Role ist eine Rolle: ein Agent mit Modell, Rechten und Prompt.
type Role struct {
	Adapter string `yaml:"adapter" json:"adapter,omitempty"`
	// Model ist ein Alias (models in config.yaml) oder eine Modell-ID.
	Model string `yaml:"model" json:"model,omitempty"`
	// Permissions: read (Workspace nur lesend) oder write (Standard).
	Permissions    string `yaml:"permissions" json:"permissions"`
	PermissionMode string `yaml:"permission_mode" json:"permission_mode,omitempty"`
	// Prompt ist eine Datei relativ zur Fabrik (z. B. prompts/planner.md).
	Prompt string `yaml:"prompt" json:"prompt_file,omitempty"`
	// PromptText ist ihr Inhalt (beim Laden gelesen).
	PromptText string   `yaml:"-" json:"prompt,omitempty"`
	Inputs     []string `yaml:"inputs" json:"inputs,omitempty"`
	// Outputs sind die Artefakte der Rolle: höchstens eins, die letzte Antwort.
	Outputs []Output `yaml:"outputs" json:"outputs,omitempty"`
	// Sandbox gibt es nur für den ganzen Run (ein Workspace); hier nur für eine klare
	// Fehlermeldung.
	Sandbox any `yaml:"sandbox" json:"-"`
}

// ReadOnly meldet, ob die Rolle den Workspace nur lesen darf.
func (r *Role) ReadOnly() bool { return r.Permissions == PermRead }

// Output liefert das Artefakt der Rolle (leer: keins).
func (r *Role) Output() string {
	if len(r.Outputs) == 0 {
		return ""
	}
	return r.Outputs[0].Name
}

// OutputSpec liefert das Artefakt der Rolle mit Schema (nil: keins).
func (r *Role) OutputSpec() *Output {
	if len(r.Outputs) == 0 {
		return nil
	}
	return &r.Outputs[0]
}

// Output ist ein Artefakt einer Rolle. In YAML ein Dateiname oder {name, schema}.
type Output struct {
	Name string `yaml:"name" json:"name"`
	// Schema ist eine Datei relativ zur Fabrik (JSON Schema, Teilmenge: Schema).
	Schema string `yaml:"schema" json:"schema_file,omitempty"`
	// SchemaDoc ist ihr Inhalt als JSON (beim Laden gelesen).
	SchemaDoc json.RawMessage `yaml:"-" json:"schema,omitempty"`
}

// Structured meldet, ob das Artefakt JSON ist (Endung .json oder Schema).
func (o *Output) Structured() bool {
	return o != nil && (strings.HasSuffix(o.Name, ".json") || o.Schema != "")
}

// UnmarshalYAML nimmt einen Dateinamen oder ein Objekt {name, schema}.
func (o *Output) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*o = Output{Name: n.Value}
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("Zeile %d: ein Artefakt ist ein Dateiname oder {name, schema}", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "name", "schema":
		default:
			return fmt.Errorf("Zeile %d: Feld %s gibt es bei einem Artefakt nicht (erlaubt: name, schema)", n.Content[i].Line, k)
		}
	}
	type plain Output
	return n.Decode((*plain)(o))
}

// UnmarshalJSON nimmt auch die Form vor M10 (nur der Name) an.
func (o *Output) UnmarshalJSON(b []byte) error {
	var name string
	if json.Unmarshal(b, &name) == nil {
		*o = Output{Name: name}
		return nil
	}
	type plain Output
	return json.Unmarshal(b, (*plain)(o))
}

// Step ist ein Schritt im Ablauf.
type Step struct {
	Step   string `yaml:"step" json:"step"`
	Role   string `yaml:"role" json:"role,omitempty"`
	Gate   string `yaml:"gate" json:"gate,omitempty"`
	Action string `yaml:"action" json:"action,omitempty"`
	// When ist eine Bedingung (Expr): falsch → Schritt übersprungen; bei einer Entscheidung
	// wahr → Rücksprung nach Goto.
	When string `yaml:"when" json:"when,omitempty"`
	Goto string `yaml:"goto" json:"goto,omitempty"`
	// MaxIterations begrenzt die Durchläufe einer Schleife (Entscheidung oder Gate mit
	// on_deny; 0: DefaultMaxIterations).
	MaxIterations int `yaml:"max_iterations" json:"max_iterations,omitempty"`
	// OnExhausted: fail (Standard) oder continue, wenn eine Entscheidung nach
	// max_iterations Durchläufen noch zurückspringen würde.
	OnExhausted string `yaml:"on_exhausted" json:"on_exhausted,omitempty"`
	// OnDeny lässt ein Gate beim Ablehnen zurückspringen statt den Run abzubrechen.
	OnDeny *OnDeny `yaml:"on_deny" json:"on_deny,omitempty"`
	// Parallel sind Rollen-Schritte, die gleichzeitig laufen (Gruppe ohne eigenen Namen).
	Parallel []Step `yaml:"parallel" json:"parallel,omitempty"`
}

// OnDeny ist der Rücksprung eines Gates beim Ablehnen.
type OnDeny struct {
	Goto string `yaml:"goto" json:"goto"`
}

// Kind liefert die Art des Schritts.
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

// Iterations liefert die erlaubten Durchläufe der Schleife des Schritts.
func (s *Step) Iterations() int {
	if s.MaxIterations > 0 {
		return s.MaxIterations
	}
	return DefaultMaxIterations
}

// Target liefert das Ziel eines Rücksprungs (Entscheidung: goto, Gate: on_deny.goto).
func (s *Step) Target() string {
	if s.OnDeny != nil {
		return s.OnDeny.Goto
	}
	return s.Goto
}

// Pos liefert die Position des Schritts name im flachen Ablauf (-1: gibt es nicht).
func (f *Factory) Pos(name string) int {
	for _, s := range f.Flat() {
		if s.Step.Step == name {
			return s.Pos
		}
	}
	return -1
}

// Budget begrenzt einen Run.
type Budget struct {
	MaxCostUSD float64 `yaml:"max_cost_usd" json:"max_cost_usd,omitempty"`
	// MaxDuration ist die Arbeitszeit der Agenten (Warten an Gates zählt nicht).
	MaxDuration time.Duration `yaml:"max_duration" json:"max_duration_ns,omitempty"`
}

// Error ist eine ungültige Fabrik.
type Error struct {
	// Path ist die Datei (z. B. .ade/factories/feature.yaml).
	Path string
	Msg  string
}

func (e *Error) Error() string { return e.Path + ": " + e.Msg }

var (
	nameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	artifactRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
)

// ValidName meldet, ob name ein gültiger Name für Fabrik, Schritt oder Rolle ist.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Parse liest eine Fabrik aus data. path ist der Pfad für Fehlermeldungen; read liest eine
// Datei (Prompt, Schema) relativ zur Fabrik (nil: Dateien werden nicht gelesen).
func Parse(path string, data []byte, read func(rel string) ([]byte, error)) (*Factory, error) {
	fail := func(format string, args ...any) error { return &Error{Path: path, Msg: fmt.Sprintf(format, args...)} }
	if len(data) > maxFileSize {
		return nil, fail("Datei ist zu groß (höchstens %d KiB)", maxFileSize>>10)
	}
	var f Factory
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fail("Datei ist leer")
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
				return nil, fail("roles.%s.prompt: Datei ist zu groß (höchstens %d KiB)", name, maxPromptSize>>10)
			}
			r.PromptText = strings.TrimSpace(string(b))
		}
		// Felder in Bedingungen erst jetzt gegen die gelesenen Schemas prüfen.
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

// loadSchemas liest und prüft die Schemas der Artefakte einer Rolle.
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
			return fmt.Errorf("%s: Datei ist zu groß (höchstens %d KiB)", at, maxSchemaSize>>10)
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

// schemaJSON liest ein Schema in JSON oder YAML und liefert es als JSON.
func schemaJSON(b []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("weder JSON noch YAML: %s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("lässt sich nicht als JSON darstellen: %w", err)
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
		return fmt.Sprintf("name: %q ist ungültig (Kleinbuchstaben, Ziffern, - und _)", f.Name)
	}
	if len(f.Description) > 500 {
		return "description: höchstens 500 Zeichen"
	}
	for _, name := range sortedKeys(f.Inputs) {
		in := f.Inputs[name]
		if !ValidName(name) {
			return fmt.Sprintf("inputs.%s: ungültiger Name", name)
		}
		if in.Type == "" {
			in.Type = InputText
			f.Inputs[name] = in
		}
		switch in.Type {
		case InputText:
		case InputBranch:
			if b := f.BranchInput(); b != name {
				return fmt.Sprintf("inputs.%s.type: höchstens eine Eingabe vom Typ branch (schon %s)", name, b)
			}
		default:
			return fmt.Sprintf("inputs.%s.type: %q wird nicht unterstützt (erlaubt: text, branch)", name, in.Type)
		}
	}
	if len(f.Roles) == 0 {
		return "roles: mindestens eine Rolle nötig"
	}
	outputs := map[string]string{} // Artefakt → Rolle
	for _, name := range f.roleNames() {
		r := f.Roles[name]
		at := "roles." + name
		if r == nil {
			return at + ": leer"
		}
		if !ValidName(name) {
			return at + ": ungültiger Name (Kleinbuchstaben, Ziffern, - und _)"
		}
		if r.Sandbox != nil {
			return at + ".sandbox: gilt für den ganzen Run (alle Rollen arbeiten im selben Workspace) und wird beim Start gewählt"
		}
		if r.Adapter == "" {
			r.Adapter = "claude"
		}
		if !slices.Contains(Adapters, r.Adapter) {
			return fmt.Sprintf("%s.adapter: %q ist unbekannt (erlaubt: %s)", at, r.Adapter, strings.Join(Adapters, ", "))
		}
		if r.Permissions == "" {
			r.Permissions = PermWrite
		}
		if r.Permissions != PermRead && r.Permissions != PermWrite {
			return fmt.Sprintf("%s.permissions: %q ist ungültig (erlaubt: read, write)", at, r.Permissions)
		}
		if strings.ContainsAny(r.Model, " \n\x00") || len(r.Model) > 200 {
			return at + ".model: ungültig"
		}
		if r.Prompt != "" {
			if msg := checkRel(r.Prompt); msg != "" {
				return at + ".prompt: " + msg
			}
		}
		if len(r.Outputs) > 1 {
			return at + ".outputs: höchstens ein Artefakt je Rolle (die letzte Antwort der Rolle)"
		}
		for _, o := range r.Outputs {
			if !artifactRE.MatchString(o.Name) {
				return fmt.Sprintf("%s.outputs: %q ist kein gültiger Dateiname", at, o.Name)
			}
			if other, ok := outputs[o.Name]; ok {
				return fmt.Sprintf("%s.outputs: %s erzeugt schon die Rolle %s", at, o.Name, other)
			}
			if o.Schema != "" {
				if !strings.HasSuffix(o.Name, ".json") {
					return fmt.Sprintf("%s.outputs: %s hat ein Schema und muss auf .json enden", at, o.Name)
				}
				if msg := checkRel(o.Schema); msg != "" {
					return fmt.Sprintf("%s.outputs.%s.schema: %s", at, o.Name, msg)
				}
			}
			outputs[o.Name] = name
		}
		for _, in := range r.Inputs {
			if !artifactRE.MatchString(in) {
				return fmt.Sprintf("%s.inputs: %q ist kein gültiger Dateiname", at, in)
			}
		}
	}
	for _, in := range f.roleNames() {
		for _, a := range f.Roles[in].Inputs {
			if _, ok := outputs[a]; !ok {
				return fmt.Sprintf("roles.%s.inputs: keine Rolle erzeugt %s", in, a)
			}
		}
	}

	if msg := f.validateFlow(); msg != "" {
		return msg
	}
	if b := f.Budget; b != nil {
		if b.MaxCostUSD < 0 || b.MaxCostUSD > MaxBudgetUSD || b.MaxCostUSD != b.MaxCostUSD {
			return fmt.Sprintf("budget.max_cost_usd: zwischen 0 und %.0f", MaxBudgetUSD)
		}
		if b.MaxDuration < 0 || b.MaxDuration > 30*24*time.Hour {
			return "budget.max_duration: zwischen 0 und 720h"
		}
	}
	return ""
}

// Grenzen für parallele Gruppen.
const (
	minParallel = 2
	maxParallel = 8
)

// FlatStep ist ein Schritt im flachen Ablauf: Mitglieder paralleler Gruppen stehen
// nacheinander, Group ist die Position des ersten Mitglieds (-1: keine Gruppe).
type FlatStep struct {
	*Step
	Pos   int
	Group int
}

// Start liefert die Position, ab der der Schritt läuft (Gruppenanfang oder eigene Position).
func (s FlatStep) Start() int {
	if s.Group >= 0 {
		return s.Group
	}
	return s.Pos
}

// Flat liefert den Ablauf flach (so legt ihn ein Run an).
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

// validateFlow prüft den Ablauf: Schritte, Gruppen, Bedingungen, Rücksprünge und dass jede
// Rolle ihre Eingaben bekommt (im ersten Durchlauf oder nach einem Rücksprung).
func (f *Factory) validateFlow() string {
	if len(f.Flow) == 0 {
		return "flow: mindestens ein Schritt nötig"
	}
	for i := range f.Flow {
		s := &f.Flow[i]
		if len(s.Parallel) == 0 {
			continue
		}
		at := fmt.Sprintf("flow[%d].parallel", i)
		if s.Step != "" || s.Role != "" || s.Gate != "" || s.Action != "" || s.Goto != "" || s.When != "" ||
			s.OnDeny != nil || s.MaxIterations != 0 || s.OnExhausted != "" {
			return at + ": eine parallele Gruppe hat nur parallel (Bedingungen an die einzelnen Schritte)"
		}
		if len(s.Parallel) < minParallel || len(s.Parallel) > maxParallel {
			return fmt.Sprintf("%s: %d bis %d Schritte", at, minParallel, maxParallel)
		}
		writers := 0
		for j := range s.Parallel {
			m := &s.Parallel[j]
			mat := fmt.Sprintf("%s[%d]", at, j)
			if len(m.Parallel) > 0 {
				return mat + ": parallele Gruppen lassen sich nicht verschachteln"
			}
			if m.Role == "" || m.Gate != "" || m.Action != "" || m.Goto != "" || m.OnDeny != nil || m.MaxIterations != 0 || m.OnExhausted != "" {
				return mat + ": parallel laufen nur Rollen (role, optional when)"
			}
			if r := f.Roles[m.Role]; r != nil && !r.ReadOnly() {
				writers++
			}
		}
		if writers > 1 {
			return at + ": höchstens eine Rolle mit Schreibrechten je Gruppe (alle arbeiten im selben Workspace)"
		}
	}
	flat := f.Flat()
	if len(flat) > maxSteps {
		return fmt.Sprintf("flow: höchstens %d Schritte", maxSteps)
	}
	pos := map[string]int{}
	label := func(s FlatStep) string { return fmt.Sprintf("flow (%s)", s.Step.Step) }
	for _, s := range flat {
		if !ValidName(s.Step.Step) {
			return fmt.Sprintf("flow: Schritt %q ist ungültig (Kleinbuchstaben, Ziffern, - und _)", s.Step.Step)
		}
		if _, ok := pos[s.Step.Step]; ok {
			return fmt.Sprintf("flow: Schritt %s gibt es schon", s.Step.Step)
		}
		pos[s.Step.Step] = s.Pos
	}
	// Rücksprünge: Herkunft → Ziel (für Eingaben, die erst ein späterer Schritt erzeugt).
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
			return at + ": genau eins von role, gate, action oder goto angeben"
		}
		if n == 0 && s.Goto == "" {
			if s.When != "" {
				return at + ": eine Entscheidung braucht goto (Rücksprung, wenn when wahr ist)"
			}
			return at + ": genau eins von role, gate, action oder goto angeben"
		}
		kind := s.Kind()
		if s.When != "" {
			if msg := f.checkWhen(fs, s.When); msg != "" {
				return at + ".when: " + msg
			}
		} else if kind == KindDecision {
			return at + ".when: eine Entscheidung braucht eine Bedingung"
		}
		if s.OnDeny != nil && kind != KindGate {
			return at + ".on_deny: gibt es nur bei Gates"
		}
		if s.OnExhausted != "" && kind != KindDecision {
			return at + ".on_exhausted: gibt es nur bei Entscheidungen (goto)"
		}
		if s.MaxIterations != 0 && kind != KindDecision && s.OnDeny == nil {
			return at + ".max_iterations: gibt es nur bei Entscheidungen (goto) und Gates mit on_deny"
		}
		if s.MaxIterations < 0 || s.MaxIterations > MaxIterations {
			return fmt.Sprintf("%s.max_iterations: zwischen 1 und %d", at, MaxIterations)
		}
		if t := s.Target(); t != "" {
			field := "goto"
			if s.OnDeny != nil {
				field = "on_deny.goto"
			}
			p, ok := pos[t]
			if !ok {
				return fmt.Sprintf("%s.%s: Schritt %s gibt es nicht", at, field, t)
			}
			to := flat[p].Start()
			if to >= fs.Pos {
				return fmt.Sprintf("%s.%s: nur Rücksprünge auf frühere Schritte (%s liegt nicht davor)", at, field, t)
			}
			loops = append(loops, loop{from: fs.Pos, to: to})
		}
		switch s.OnExhausted {
		case "", ExhaustedFail, ExhaustedContinue:
		default:
			return fmt.Sprintf("%s.on_exhausted: %q ist ungültig (erlaubt: fail, continue)", at, s.OnExhausted)
		}
		switch kind {
		case KindRole:
			if f.Roles[s.Role] == nil {
				return fmt.Sprintf("%s.role: Rolle %s ist nicht definiert", at, s.Role)
			}
		case KindGate:
			if s.Gate != GateHuman {
				return fmt.Sprintf("%s.gate: %q ist ungültig (erlaubt: human)", at, s.Gate)
			}
		case KindAction:
			if s.Action != ActionPullRequest {
				return fmt.Sprintf("%s.action: %q ist ungültig (erlaubt: pull_request)", at, s.Action)
			}
			if fs.Pos != len(flat)-1 {
				return at + ".action: pull_request muss der letzte Schritt sein"
			}
		}
	}
	// Eingaben: im ersten Durchlauf von einem früheren Schritt erzeugt (nicht aus derselben
	// Gruppe) oder durch einen Rücksprung von hinter dem Erzeuger erreichbar.
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
				return fmt.Sprintf("%s: die Rolle %s braucht %s, das erst ein späterer Schritt erzeugt (und kein Rücksprung führt zurück)", label(s), s.Role, a)
			}
		}
	}
	return ""
}

// checkWhen prüft die Bedingung eines Schritts: Jeder Pfad nennt einen früheren Schritt (nicht
// aus derselben Gruppe) mit strukturiertem Artefakt und, falls das Schema Felder nennt, eines
// davon.
func (f *Factory) checkWhen(at FlatStep, src string) string {
	e, err := ParseExpr(src)
	if err != nil {
		return err.Error()
	}
	flat := f.Flat()
	for _, path := range e.Paths() {
		p := f.Pos(path[0])
		if p < 0 {
			return fmt.Sprintf("Schritt %s gibt es nicht", path[0])
		}
		if p >= at.Start() {
			return fmt.Sprintf("%s ist kein früherer Schritt", path[0])
		}
		var out *Output
		if r := f.Roles[flat[p].Role]; r != nil {
			out = r.OutputSpec()
		}
		if !out.Structured() {
			return fmt.Sprintf("der Schritt %s hat kein strukturiertes Artefakt (outputs mit .json)", path[0])
		}
		if len(out.SchemaDoc) > 0 {
			if sch, err := CompileSchema(out.SchemaDoc); err == nil && len(sch.Properties) > 0 && sch.Properties[path[1]] == nil {
				return fmt.Sprintf("%s hat laut Schema kein Feld %s", out.Name, path[1])
			}
		}
	}
	return ""
}

// checkRel prüft einen Pfad relativ zur Fabrik-Datei: kein absoluter Pfad, kein „..“.
func checkRel(p string) string {
	if strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return fmt.Sprintf("%q muss ein relativer Pfad sein", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Sprintf("%q darf nicht aus %s herausführen", p, Dir)
		}
	}
	return ""
}

// BranchInput liefert den Namen der Eingabe vom Typ branch (leer: keine).
func (f *Factory) BranchInput() string {
	for _, k := range sortedKeys(f.Inputs) {
		if f.Inputs[k].Type == InputBranch {
			return k
		}
	}
	return ""
}

// CheckInputs prüft die Eingaben eines Runs und liefert sie bereinigt.
func (f *Factory) CheckInputs(in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range in {
		if _, ok := f.Inputs[k]; !ok {
			return nil, fmt.Errorf("Eingabe %s gibt es in der Fabrik %s nicht", k, f.Name)
		}
		v = strings.TrimSpace(v)
		if len(v) > 100<<10 {
			return nil, fmt.Errorf("Eingabe %s ist zu lang (höchstens 100 KiB)", k)
		}
		if v != "" {
			out[k] = v
		}
	}
	for _, k := range sortedKeys(f.Inputs) {
		if f.Inputs[k].Required && out[k] == "" {
			return nil, fmt.Errorf("Eingabe %s fehlt", k)
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
