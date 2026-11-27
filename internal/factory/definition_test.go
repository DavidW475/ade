package factory

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuiltinFeature(t *testing.T) {
	f, src, err := Load(t.TempDir(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceBuiltin {
		t.Fatalf("Quelle %s", src)
	}
	var kinds []string
	for _, s := range f.Flow {
		kinds = append(kinds, s.Step+":"+s.Kind())
	}
	if got := strings.Join(kinds, " "); got != "plan:role approve-plan:gate implement:role review:role final:gate deliver:action" {
		t.Fatalf("Ablauf %s", got)
	}
	if !f.Roles["planner"].ReadOnly() || f.Roles["implementer"].ReadOnly() || !f.Roles["reviewer"].ReadOnly() {
		t.Fatal("Rechte der Rollen")
	}
	if !strings.Contains(f.Roles["planner"].PromptText, "Planer") {
		t.Fatalf("Prompt nicht geladen: %q", f.Roles["planner"].PromptText)
	}
	if f.Budget == nil || f.Budget.MaxDuration != 2*time.Hour {
		t.Fatalf("Budget %+v", f.Budget)
	}
}

const minimal = `name: x
roles:
  a: { permissions: read, outputs: [plan.md] }
  b: { inputs: [plan.md] }
flow:
  - { step: one, role: a }
  - { step: two, role: b }
`

func TestParseValid(t *testing.T) {
	f, err := Parse("x.yaml", []byte(minimal), nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Roles["b"].Adapter != "claude" || f.Roles["b"].Permissions != PermWrite {
		t.Fatalf("Voreinstellungen: %+v", f.Roles["b"])
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"unbekanntes Feld": {strings.Replace(minimal, "name: x", "name: x\nfoo: 1", 1), "field foo not found"},
		"Name":             {strings.Replace(minimal, "name: x", "name: X Y", 1), "name:"},
		"Rolle fehlt":      {strings.Replace(minimal, "role: b", "role: c", 1), "Rolle c ist nicht definiert"},
		"Reihenfolge": {`name: x
roles:
  a: { permissions: read, outputs: [plan.md] }
  b: { inputs: [plan.md] }
flow:
  - { step: two, role: b }
  - { step: one, role: a }
`, "erst ein späterer Schritt"},
		"Eingabe ohne Erzeuger": {strings.Replace(minimal, "inputs: [plan.md]", "inputs: [x.md]", 1), "keine Rolle erzeugt x.md"},
		"doppelter Schritt":     {strings.Replace(minimal, "step: two", "step: one", 1), "one gibt es schon"},
		"parallel": {minimal + `  - parallel:
      - { step: r, role: a }
`, "erst ab M10"},
		"when":              {minimal + "  - { step: d, when: \"x\", goto: one }\n", "erst ab M10"},
		"gate":              {minimal + "  - { step: g, gate: robot }\n", "gate: \"robot\""},
		"zwei Arten":        {minimal + "  - { step: g, gate: human, role: a }\n", "genau eins"},
		"PR nicht am Ende":  {strings.Replace(minimal, "  - { step: two, role: b }\n", "  - { step: pr, action: pull_request }\n  - { step: two, role: b }\n", 1), "letzte Schritt"},
		"Adapter":           {strings.Replace(minimal, "a: { permissions: read", "a: { adapter: pty, permissions: read", 1), "adapter: \"pty\""},
		"Rechte":            {strings.Replace(minimal, "permissions: read", "permissions: admin", 1), "permissions"},
		"sandbox":           {strings.Replace(minimal, "b: { inputs", "b: { sandbox: default, inputs", 1), "ganzen Run"},
		"Prompt":            {strings.Replace(minimal, "b: { inputs", "b: { prompt: ../x.md, inputs", 1), "herausführen"},
		"zwei Ausgaben":     {strings.Replace(minimal, "outputs: [plan.md]", "outputs: [plan.md, b.json]", 1), "höchstens ein Artefakt"},
		"Schema ohne .json": {strings.Replace(minimal, "outputs: [plan.md]", "outputs: [{ name: plan.md, schema: s.json }]", 1), "muss auf .json enden"},
		"Schema außerhalb":  {strings.Replace(minimal, "outputs: [plan.md]", "outputs: [{ name: p.json, schema: ../s.json }]", 1), "herausführen"},
		"Artefakt-Feld":     {strings.Replace(minimal, "outputs: [plan.md]", "outputs: [{ name: plan.md, type: x }]", 1), "Feld type gibt es bei einem Artefakt nicht"},
		"leer":              {"", "leer"},
		"Budget":            {minimal + "budget: { max_cost_usd: -1 }\n", "max_cost_usd"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(".ade/factories/x.yaml", []byte(c.yaml), nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Fehler %v, erwartet %q", err, c.want)
			}
			if !strings.HasPrefix(err.Error(), ".ade/factories/x.yaml: ") {
				t.Fatalf("Datei fehlt in %v", err)
			}
		})
	}
}

func writeFile(t *testing.T, p, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProject(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, Dir)
	writeFile(t, filepath.Join(dir, "feature.yaml"), strings.Replace(minimal, "name: x", "name: feature", 1)+"description: eigene\n")
	writeFile(t, filepath.Join(dir, "p.yaml"), "name: p\nroles:\n  a: { prompt: prompts/a.md }\nflow:\n  - { step: s, role: a }\n")
	writeFile(t, filepath.Join(dir, "prompts", "a.md"), "  Hallo  \n")
	writeFile(t, filepath.Join(dir, "kaputt.yaml"), "name: kaputt\n")
	writeFile(t, filepath.Join(repo, "geheim.md"), "geheim")
	if err := os.Symlink(filepath.Join(repo, "geheim.md"), filepath.Join(dir, "prompts", "link.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "link.yaml"), "name: link\nroles:\n  a: { prompt: prompts/link.md }\nflow:\n  - { step: s, role: a }\n")

	f, src, err := Load(repo, "feature")
	if err != nil || src != SourceProject || f.Description != "eigene" {
		t.Fatalf("eigene Fabrik statt Vorlage: %v %s %+v", err, src, f)
	}
	p, _, err := Load(repo, "p")
	if err != nil || p.Roles["a"].PromptText != "Hallo" {
		t.Fatalf("Prompt: %v %+v", err, p)
	}
	if _, _, err := Load(repo, "link"); err == nil {
		t.Fatal("Symlink aus .ade/factories hinaus wurde gelesen")
	}
	if _, _, err := Load(repo, "fehlt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fehlende Fabrik: %v", err)
	}
	if _, _, err := Load(repo, "../x"); err == nil {
		t.Fatal("ungültiger Name angenommen")
	}

	list := List(repo)
	byName := map[string]Summary{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if byName["feature"].Source != SourceProject || byName["kaputt"].Error == "" || byName["p"].Error != "" {
		t.Fatalf("Liste %+v", list)
	}
	if len(byName["p"].Steps) != 1 || byName["p"].Steps[0].Kind != KindRole {
		t.Fatalf("Schritte %+v", byName["p"].Steps)
	}
}

func TestLoadSchema(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, Dir)
	def := "name: s\nroles:\n  r: { permissions: read, outputs: [{ name: review.json, schema: schemas/review.%s }] }\n" +
		"  w: { inputs: [review.json] }\nflow:\n  - { step: review, role: r }\n  - { step: fix, role: w }\n"
	writeFile(t, filepath.Join(dir, "s.yaml"), strings.Replace(def, "%s", "json", 1))
	writeFile(t, filepath.Join(dir, "schemas", "review.json"), reviewSchema)
	writeFile(t, filepath.Join(dir, "y.yaml"), strings.Replace(strings.Replace(def, "%s", "yaml", 1), "name: s", "name: y", 1))
	writeFile(t, filepath.Join(dir, "schemas", "review.yaml"), "type: object\nrequired: [verdict]\n")
	writeFile(t, filepath.Join(dir, "bad.yaml"), strings.Replace(strings.Replace(def, "%s", "bad", 1), "name: s", "name: bad", 1))
	writeFile(t, filepath.Join(dir, "schemas", "review.bad"), `{"type": "object", "oneOf": []}`)

	f, _, err := Load(repo, "s")
	if err != nil {
		t.Fatal(err)
	}
	out := f.Roles["r"].OutputSpec()
	if !out.Structured() || out.Name != "review.json" || !strings.Contains(string(out.SchemaDoc), "approve") {
		t.Fatalf("Artefakt %+v", out)
	}
	// Die Definition wird als JSON im Run gespeichert und muss sich wieder lesen lassen.
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back Factory
	if err := json.Unmarshal(b, &back); err != nil || string(back.Roles["r"].OutputSpec().SchemaDoc) != string(out.SchemaDoc) {
		t.Fatalf("JSON: %v %s", err, b)
	}
	// Definitionen von vor M10 (outputs als Liste von Namen).
	var old Factory
	if err := json.Unmarshal([]byte(`{"roles":{"a":{"outputs":["plan.md"]}}}`), &old); err != nil || old.Roles["a"].Output() != "plan.md" {
		t.Fatalf("alte Definition: %v %+v", err, old.Roles["a"])
	}
	if y, _, err := Load(repo, "y"); err != nil || !strings.Contains(string(y.Roles["r"].OutputSpec().SchemaDoc), `"required":["verdict"]`) {
		t.Fatalf("YAML-Schema: %v", err)
	}
	if _, _, err := Load(repo, "bad"); err == nil || !strings.Contains(err.Error(), "oneOf wird nicht unterstützt") {
		t.Fatalf("ungültiges Schema: %v", err)
	}
}

func TestCheckInputs(t *testing.T) {
	f, _, err := Load(t.TempDir(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.CheckInputs(map[string]string{"task": "  "}); err == nil {
		t.Fatal("leere Pflicht-Eingabe angenommen")
	}
	if _, err := f.CheckInputs(map[string]string{"task": "x", "foo": "y"}); err == nil {
		t.Fatal("unbekannte Eingabe angenommen")
	}
	in, err := f.CheckInputs(map[string]string{"task": " bau was "})
	if err != nil || in["task"] != "bau was" {
		t.Fatalf("%v %v", in, err)
	}
}
