package factory

import (
	"encoding/json"
	"errors"
	"fmt"
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
		k := s.Kind()
		if len(s.Parallel) > 0 {
			k = "parallel"
		}
		kinds = append(kinds, s.Step+":"+k)
	}
	if got := strings.Join(kinds, " "); got != "plan:role approve-plan:gate implement:role :parallel decide:decision final:gate deliver:action" {
		t.Fatalf("Ablauf %s", got)
	}
	if !f.Roles["planner"].ReadOnly() || f.Roles["implementer"].ReadOnly() || !f.Roles["reviewer"].ReadOnly() {
		t.Fatal("permissions of the roles")
	}
	if !strings.Contains(f.Roles["planner"].PromptText, "planner") {
		t.Fatalf("prompt not loaded: %q", f.Roles["planner"].PromptText)
	}
	if f.Budget == nil || f.Budget.MaxDuration != 2*time.Hour {
		t.Fatalf("Budget %+v", f.Budget)
	}
}

// TestBuiltinTemplates: all templates load without errors, with prompts and schemas.
func TestBuiltinTemplates(t *testing.T) {
	names := builtinNames()
	if strings.Join(names, " ") != "bugfix feature refactor review-only" {
		t.Fatalf("Vorlagen %v", names)
	}
	for _, name := range names {
		f, err := loadBuiltin(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if f.Name != name || f.Description == "" || f.Budget == nil || f.Budget.MaxCostUSD <= 0 {
			t.Errorf("%s: name, description or budget missing", name)
		}
		for rn, r := range f.Roles {
			if r.PromptText == "" {
				t.Errorf("%s: role %s without prompt", name, rn)
			}
			if o := r.OutputSpec(); o.Structured() && len(o.SchemaDoc) == 0 {
				t.Errorf("%s: %s without schema", name, o.Name)
			}
		}
	}
	ro, _ := loadBuiltin("review-only")
	if ro.BranchInput() != "branch" || ro.Inputs["branch"].Required {
		t.Fatalf("review-only: input branch %+v", ro.Inputs)
	}
	if last := ro.Flat()[len(ro.Flat())-1]; last.Kind() != KindRole {
		t.Fatal("review-only does not end with the report")
	}
}

func TestBranchInput(t *testing.T) {
	two := "name: x\ninputs:\n  a: { type: branch }\n  b: { type: branch }\nroles:\n  r: {}\nflow:\n  - { step: s, role: r }\n"
	if _, err := Parse("x.yaml", []byte(two), nil); err == nil || !strings.Contains(err.Error(), "at most one input of type branch") {
		t.Fatalf("two branch inputs: %v", err)
	}
	if _, err := Parse("x.yaml", []byte(strings.Replace(two, "  b: { type: branch }\n", "  b: { type: number }\n", 1)), nil); err == nil || !strings.Contains(err.Error(), "allowed: text, branch") {
		t.Fatalf("unknown type: %v", err)
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
		"unknown field": {strings.Replace(minimal, "name: x", "name: x\nfoo: 1", 1), "field foo not found"},
		"name":          {strings.Replace(minimal, "name: x", "name: X Y", 1), "name:"},
		"role missing":  {strings.Replace(minimal, "role: b", "role: c", 1), "role c is not defined"},
		"Reihenfolge": {`name: x
roles:
  a: { permissions: read, outputs: [plan.md] }
  b: { inputs: [plan.md] }
flow:
  - { step: two, role: b }
  - { step: one, role: a }
`, "only a later step produces"},
		"input without producer": {strings.Replace(minimal, "inputs: [plan.md]", "inputs: [x.md]", 1), "no role produces x.md"},
		"duplicate step":         {strings.Replace(minimal, "step: two", "step: one", 1), "one already exists"},
		"parallel": {minimal + `  - parallel:
      - { step: r, role: a }
`, "2 to 8 steps"},
		"parallel Gate": {minimal + `  - parallel:
      - { step: r, role: a }
      - { step: g, gate: human }
`, "only roles"},
		"parallel two writers": {minimal + `  - parallel:
      - { step: r, role: b }
      - { step: s, role: b }
`, "at most one role with write permission"},
		"parallel with name": {minimal + `  - step: grp
    parallel:
      - { step: r, role: a }
      - { step: s, role: a }
`, "only has parallel"},
		"parallel Geschwister": {`name: x
roles:
  a: { permissions: read, outputs: [a.json] }
  b: { permissions: read, inputs: [a.json] }
flow:
  - parallel:
      - { step: one, role: a }
      - { step: two, role: b }
`, "only a later step produces"},
		"when without artifact": {minimal + "  - { step: d, when: \"one.x == 1\", goto: one }\n", "no structured artifact"},
		"when Syntax":           {minimal + "  - { step: d, when: \"one.x = 1\", goto: one }\n", "write comparisons with =="},
		"when later":            {strings.Replace(minimal, "  - { step: one, role: a }\n", "  - { step: one, role: a, when: \"two.x == 1\" }\n", 1), "is not an earlier step"},
		"when unknown":          {minimal + "  - { step: d, when: \"zz.x == 1\", goto: one }\n", "step zz does not exist"},
		"decision without when": {minimal + "  - { step: d, goto: one }\n", "needs a condition"},
		"when without goto":     {minimal + "  - { step: d, when: \"one.x == 1\" }\n", "needs goto"},
		"goto forward":          {strings.ReplaceAll(minimal, "plan.md", "p.json") + "  - { step: d, when: \"one.x\", goto: d }\n", "only jumps back"},
		"goto unknown":          {strings.ReplaceAll(minimal, "plan.md", "p.json") + "  - { step: d, when: \"one.x\", goto: zz }\n", "step zz does not exist"},
		"goto on role":          {strings.Replace(minimal, "  - { step: two, role: b }", "  - { step: two, role: b, goto: one }", 1), "exactly one"},
		"on_deny on role":       {strings.Replace(minimal, "  - { step: two, role: b }", "  - { step: two, role: b, on_deny: { goto: one } }", 1), "only exists for gates"},
		"max_iterations":        {minimal + "  - { step: g, gate: human, on_deny: { goto: one }, max_iterations: 50 }\n", "between 1 and 20"},
		"max_iterations gate":   {minimal + "  - { step: g, gate: human, max_iterations: 2 }\n", "only exists for decisions"},
		"on_exhausted":          {strings.ReplaceAll(minimal, "plan.md", "p.json") + "  - { step: d, when: \"one.x\", goto: one, on_exhausted: retry }\n", "on_exhausted"},
		"loop input without jump back": {`name: x
roles:
  a: { inputs: [r.json] }
  b: { permissions: read, outputs: [r.json] }
flow:
  - { step: one, role: a }
  - { step: two, role: b }
`, "no jump back leads there"},
		"gate":                 {minimal + "  - { step: g, gate: robot }\n", "gate: \"robot\""},
		"two kinds":            {minimal + "  - { step: g, gate: human, role: a }\n", "exactly one"},
		"PR without gate":      {minimal + "  - { step: pr, action: pull_request }\n", "needs a gate before it"},
		"PR after writer":      {strings.Replace(minimal, "  - { step: two, role: b }\n", "  - { step: g, gate: human }\n  - { step: two, role: b }\n  - { step: pr, action: pull_request }\n", 1), "needs a gate before it"},
		"PR gate with when":    {strings.ReplaceAll(minimal, "plan.md", "p.json") + "  - { step: g, gate: human, when: \"one.x\" }\n  - { step: pr, action: pull_request }\n", "needs a gate before it"},
		"unattended gate":      {minimal + "  - { step: g, gate: human, unattended: true }\n", "only exists for action"},
		"PR not at the end":    {strings.Replace(minimal, "  - { step: two, role: b }\n", "  - { step: pr, action: pull_request }\n  - { step: two, role: b }\n", 1), "last step"},
		"Adapter":              {strings.Replace(minimal, "a: { permissions: read", "a: { adapter: pty, permissions: read", 1), "adapter: \"pty\""},
		"permissions":          {strings.Replace(minimal, "permissions: read", "permissions: admin", 1), "permissions"},
		"sandbox":              {strings.Replace(minimal, "b: { inputs", "b: { sandbox: default, inputs", 1), "whole run"},
		"Prompt":               {strings.Replace(minimal, "b: { inputs", "b: { prompt: ../x.md, inputs", 1), "must not lead out"},
		"two outputs":          {strings.Replace(minimal, "outputs: [plan.md]", "outputs: [plan.md, b.json]", 1), "at most one artifact"},
		"schema without .json": {strings.Replace(minimal, "outputs: [plan.md]", "outputs: [{ name: plan.md, schema: s.json }]", 1), "must end in .json"},
		"schema outside":       {strings.Replace(minimal, "outputs: [plan.md]", "outputs: [{ name: p.json, schema: ../s.json }]", 1), "must not lead out"},
		"artifact field":       {strings.Replace(minimal, "outputs: [plan.md]", "outputs: [{ name: plan.md, type: x }]", 1), "field type does not exist for an artifact"},
		"empty":                {"", "empty"},
		"budget":               {minimal + "budget: { max_cost_usd: -1 }\n", "max_cost_usd"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(".ade/factories/x.yaml", []byte(c.yaml), nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			if !strings.HasPrefix(err.Error(), ".ade/factories/x.yaml: ") {
				t.Fatalf("file missing in %v", err)
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
	writeFile(t, filepath.Join(dir, "feature.yaml"), strings.Replace(minimal, "name: x", "name: feature", 1)+"description: own\n")
	writeFile(t, filepath.Join(dir, "p.yaml"), "name: p\nroles:\n  a: { prompt: prompts/a.md }\nflow:\n  - { step: s, role: a }\n")
	writeFile(t, filepath.Join(dir, "prompts", "a.md"), "  Hallo  \n")
	writeFile(t, filepath.Join(dir, "broken.yaml"), "name: broken\n")
	writeFile(t, filepath.Join(repo, "geheim.md"), "geheim")
	if err := os.Symlink(filepath.Join(repo, "geheim.md"), filepath.Join(dir, "prompts", "link.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "link.yaml"), "name: link\nroles:\n  a: { prompt: prompts/link.md }\nflow:\n  - { step: s, role: a }\n")

	f, src, err := Load(repo, "feature")
	if err != nil || src != SourceProject || f.Description != "own" {
		t.Fatalf("own factory instead of template: %v %s %+v", err, src, f)
	}
	p, _, err := Load(repo, "p")
	if err != nil || p.Roles["a"].PromptText != "Hallo" {
		t.Fatalf("Prompt: %v %+v", err, p)
	}
	if _, _, err := Load(repo, "link"); err == nil {
		t.Fatal("symlink out of .ade/factories was read")
	}
	if _, _, err := Load(repo, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fehlende Fabrik: %v", err)
	}
	if _, _, err := Load(repo, "../x"); err == nil {
		t.Fatal("invalid name accepted")
	}

	list := List(repo)
	byName := map[string]Summary{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if byName["feature"].Source != SourceProject || byName["broken"].Error == "" || byName["p"].Error != "" {
		t.Fatalf("Liste %+v", list)
	}
	if len(byName["p"].Steps) != 1 || byName["p"].Steps[0].Kind != KindRole {
		t.Fatalf("Schritte %+v", byName["p"].Steps)
	}
}

const loopFactory = `name: loop
roles:
  dev: { inputs: [review.json], outputs: [summary.md] }
  rev: { permissions: read, inputs: [summary.md], outputs: [review.json] }
flow:
  - { step: implement, role: dev }
  - { step: review, role: rev }
  - { step: decide, when: "review.verdict == 'changes'", goto: implement, max_iterations: 4, on_exhausted: continue }
  - { step: final, gate: human, on_deny: { goto: implement } }
  - { step: deliver, action: pull_request, when: "review.verdict != 'reject'" }
`

// TestParsePullRequestGate: pull_request after a gate (only read-only roles and decisions
// after it) or with unattended: true is valid (#103).
func TestParsePullRequestGate(t *testing.T) {
	for name, src := range map[string]string{
		"Gate davor":        minimal + "  - { step: g, gate: human }\n  - { step: pr, action: pull_request }\n",
		"reader after gate": strings.Replace(minimal, "  - { step: two, role: b }\n", "  - { step: two, role: b }\n  - { step: g, gate: human }\n  - { step: three, role: a }\n  - { step: pr, action: pull_request }\n", 1),
		"unattended":        minimal + "  - { step: pr, action: pull_request, unattended: true }\n",
	} {
		f, err := Parse("x.yaml", []byte(src), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if last := f.Flow[len(f.Flow)-1]; last.Unattended != (name == "unattended") {
			t.Fatalf("%s: unattended %v", name, last.Unattended)
		}
	}
}

func TestParseLoop(t *testing.T) {
	f, err := Parse("loop.yaml", []byte(loopFactory), nil)
	if err != nil {
		t.Fatal(err)
	}
	d := f.Flow[2]
	if d.Kind() != KindDecision || d.Target() != "implement" || d.Iterations() != 4 || f.Pos("final") != 3 {
		t.Fatalf("Entscheidung %+v", d)
	}
	if g := f.Flow[3]; g.Kind() != KindGate || g.Target() != "implement" || g.Iterations() != DefaultMaxIterations {
		t.Fatalf("Gate %+v", g)
	}
	// With a schema the fields of the condition are checked.
	read := func(string) ([]byte, error) { return []byte(reviewSchema), nil }
	withSchema := strings.Replace(loopFactory, "outputs: [review.json]", "outputs: [{ name: review.json, schema: s.json }]", 1)
	if _, err := Parse("loop.yaml", []byte(withSchema), read); err != nil {
		t.Fatal(err)
	}
	typo := strings.Replace(withSchema, "review.verdict == 'changes'", "review.verdikt == 'changes'", 1)
	if _, err := Parse("loop.yaml", []byte(typo), read); err == nil || !strings.Contains(err.Error(), "no field verdikt") {
		t.Fatalf("typo in the field: %v", err)
	}
}

func TestParseParallel(t *testing.T) {
	_, err := Parse("p.yaml", []byte(`name: p
roles:
  dev: { inputs: [review.json, test.json], outputs: [summary.md] }
  rev: { permissions: read, inputs: [summary.md], outputs: [review.json] }
  tst: { inputs: [summary.md], outputs: [test.json] }
flow:
  - { step: implement, role: dev }
  - parallel:
      - { step: review, role: rev }
      - { step: test, role: tst, when: "implement.x == 1" }
  - { step: decide, when: "review.verdict == 'changes' || test.failed > 0", goto: review }
`), nil)
	if err == nil || !strings.Contains(err.Error(), "no structured artifact") {
		t.Fatalf("condition on a text artifact: %v", err)
	}
	f, err := Parse("p.yaml", []byte(`name: p
roles:
  dev: { inputs: [review.json, test.json], outputs: [summary.md] }
  rev: { permissions: read, inputs: [summary.md], outputs: [review.json] }
  tst: { inputs: [summary.md], outputs: [test.json] }
flow:
  - { step: implement, role: dev }
  - parallel:
      - { step: review, role: rev }
      - { step: test, role: tst }
  - { step: decide, when: "review.verdict == 'changes' || test.failed > 0", goto: implement }
  - { step: again, when: "test.failed > 0", goto: test }
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range f.Flat() {
		got = append(got, fmt.Sprintf("%s@%d/%d", s.Step.Step, s.Pos, s.Start()))
	}
	if strings.Join(got, " ") != "implement@0/0 review@1/1 test@2/1 decide@3/3 again@4/4" || f.Pos("test") != 2 {
		t.Fatalf("flach: %v", got)
	}
	if st := f.Steps(); st[2].Group == nil || *st[2].Group != 1 || st[0].Group != nil {
		t.Fatalf("overview %+v", st)
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
	// The definition is stored as JSON in the run and must be readable again.
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back Factory
	if err := json.Unmarshal(b, &back); err != nil || string(back.Roles["r"].OutputSpec().SchemaDoc) != string(out.SchemaDoc) {
		t.Fatalf("JSON: %v %s", err, b)
	}
	// Definitions from before M10 (outputs as a list of names).
	var old Factory
	if err := json.Unmarshal([]byte(`{"roles":{"a":{"outputs":["plan.md"]}}}`), &old); err != nil || old.Roles["a"].Output() != "plan.md" {
		t.Fatalf("alte Definition: %v %+v", err, old.Roles["a"])
	}
	if y, _, err := Load(repo, "y"); err != nil || !strings.Contains(string(y.Roles["r"].OutputSpec().SchemaDoc), `"required":["verdict"]`) {
		t.Fatalf("YAML-Schema: %v", err)
	}
	if _, _, err := Load(repo, "bad"); err == nil || !strings.Contains(err.Error(), "oneOf is not supported") {
		t.Fatalf("invalid schema: %v", err)
	}
}

func TestCheckInputs(t *testing.T) {
	f, _, err := Load(t.TempDir(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.CheckInputs(map[string]string{"task": "  "}); err == nil {
		t.Fatal("empty required input accepted")
	}
	if _, err := f.CheckInputs(map[string]string{"task": "x", "foo": "y"}); err == nil {
		t.Fatal("unknown input accepted")
	}
	in, err := f.CheckInputs(map[string]string{"task": " bau was "})
	if err != nil || in["task"] != "bau was" {
		t.Fatalf("%v %v", in, err)
	}
}
