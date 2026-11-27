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

// Input ist eine Eingabe eines Runs (M9: nur Text).
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
	// Ab M10; hier nur für eine klare Fehlermeldung.
	Parallel      []Step `yaml:"parallel" json:"-"`
	When          string `yaml:"when" json:"-"`
	Goto          string `yaml:"goto" json:"-"`
	MaxIterations int    `yaml:"max_iterations" json:"-"`
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
	}
	return ""
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
			in.Type = "text"
			f.Inputs[name] = in
		}
		if in.Type != "text" {
			return fmt.Sprintf("inputs.%s.type: %q wird nicht unterstützt (erlaubt: text)", name, in.Type)
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

	if len(f.Flow) == 0 {
		return "flow: mindestens ein Schritt nötig"
	}
	if len(f.Flow) > maxSteps {
		return fmt.Sprintf("flow: höchstens %d Schritte", maxSteps)
	}
	seen := map[string]bool{}
	produced := map[string]bool{}
	for i := range f.Flow {
		s := &f.Flow[i]
		at := fmt.Sprintf("flow[%d]", i)
		switch {
		case len(s.Parallel) > 0:
			return at + ".parallel: parallele Schritte gibt es erst ab M10"
		case s.When != "":
			return at + ".when: Bedingungen gibt es erst ab M10"
		case s.Goto != "" || s.MaxIterations != 0:
			return at + ".goto: Schleifen gibt es erst ab M10"
		}
		if !ValidName(s.Step) {
			return fmt.Sprintf("%s.step: %q ist ungültig (Kleinbuchstaben, Ziffern, - und _)", at, s.Step)
		}
		if seen[s.Step] {
			return fmt.Sprintf("%s.step: %s gibt es schon", at, s.Step)
		}
		seen[s.Step] = true
		n := 0
		for _, v := range []string{s.Role, s.Gate, s.Action} {
			if v != "" {
				n++
			}
		}
		if n != 1 {
			return at + ": genau eins von role, gate oder action angeben"
		}
		switch s.Kind() {
		case KindRole:
			r := f.Roles[s.Role]
			if r == nil {
				return fmt.Sprintf("%s.role: Rolle %s ist nicht definiert", at, s.Role)
			}
			for _, a := range r.Inputs {
				if !produced[a] {
					return fmt.Sprintf("%s: die Rolle %s braucht %s, das erst ein späterer Schritt erzeugt", at, s.Role, a)
				}
			}
			for _, o := range r.Outputs {
				produced[o.Name] = true
			}
		case KindGate:
			if s.Gate != GateHuman {
				return fmt.Sprintf("%s.gate: %q ist ungültig (erlaubt: human)", at, s.Gate)
			}
		case KindAction:
			if s.Action != ActionPullRequest {
				return fmt.Sprintf("%s.action: %q ist ungültig (erlaubt: pull_request)", at, s.Action)
			}
			if i != len(f.Flow)-1 {
				return at + ".action: pull_request muss der letzte Schritt sein"
			}
		}
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
