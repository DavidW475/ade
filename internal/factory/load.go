package factory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DavidW475/ade/examples"
)

// Herkunft einer Fabrik.
const (
	SourceProject = "project" // .ade/factories im Repo
	SourceBuiltin = "builtin" // Vorlage aus examples/factories
)

// Summary beschreibt eine verfügbare Fabrik.
type Summary struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Source      string           `json:"source"`
	Path        string           `json:"path"`
	Inputs      map[string]Input `json:"inputs,omitempty"`
	Steps       []StepSummary    `json:"steps,omitempty"`
	// Error ist gesetzt, wenn die Datei ungültig ist.
	Error string `json:"error,omitempty"`
}

// StepSummary ist ein Schritt für die Übersicht.
type StepSummary struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Role     string `json:"role,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Steps liefert die Schritte für die Übersicht.
func (f *Factory) Steps() []StepSummary {
	out := make([]StepSummary, len(f.Flow))
	for i, s := range f.Flow {
		out[i] = StepSummary{Name: s.Step, Kind: s.Kind(), Role: s.Role}
		if r := f.Roles[s.Role]; r != nil {
			out[i].ReadOnly = r.ReadOnly()
		}
	}
	return out
}

// Load lädt die Fabrik name: zuerst aus .ade/factories/<name>.yaml in repo, sonst die
// eingebaute Vorlage. Fehlt beides, ist der Fehler fs.ErrNotExist.
func Load(repo, name string) (*Factory, string, error) {
	if !ValidName(name) {
		return nil, "", fmt.Errorf("%q ist kein gültiger Fabrik-Name", name)
	}
	f, err := loadProject(repo, name)
	if err == nil {
		return f, SourceProject, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, SourceProject, err
	}
	f, err = loadBuiltin(name)
	if err != nil {
		return nil, SourceBuiltin, err
	}
	return f, SourceBuiltin, nil
}

// List liefert alle Fabriken eines Repos (eigene vor gleichnamigen Vorlagen), nach Name.
func List(repo string) []Summary {
	byName := map[string]Summary{}
	for _, name := range builtinNames() {
		f, err := loadBuiltin(name)
		byName[name] = summary(name, SourceBuiltin, "examples/factories/"+name+".yaml", f, err)
	}
	for _, name := range projectNames(repo) {
		f, err := loadProject(repo, name)
		byName[name] = summary(name, SourceProject, path.Join(Dir, name+".yaml"), f, err)
	}
	out := make([]Summary, 0, len(byName))
	for _, k := range sortedKeys(byName) {
		out = append(out, byName[k])
	}
	return out
}

func summary(name, source, p string, f *Factory, err error) Summary {
	s := Summary{Name: name, Source: source, Path: p}
	if err != nil {
		s.Error = err.Error()
		return s
	}
	s.Description, s.Inputs, s.Steps = f.Description, f.Inputs, f.Steps()
	return s
}

func projectNames(repo string) []string {
	entries, err := os.ReadDir(filepath.Join(repo, Dir))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && !e.IsDir() && ValidName(n) {
			names = append(names, n)
		}
	}
	return names
}

// loadProject liest eine Fabrik aus dem Repo. Prompts müssen unter .ade/factories liegen;
// os.Root verhindert, dass Symlinks hinausführen.
func loadProject(repo, name string) (*Factory, error) {
	root, err := os.OpenRoot(filepath.Join(repo, Dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file := name + ".yaml"
	data, err := root.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, &Error{Path: path.Join(Dir, file), Msg: err.Error()}
	}
	f, err := Parse(path.Join(Dir, file), data, func(rel string) ([]byte, error) {
		b, err := root.ReadFile(path.Clean(rel))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s fehlt", path.Join(Dir, rel))
		}
		return b, err
	})
	if err != nil {
		return nil, err
	}
	if f.Name != name {
		return nil, &Error{Path: path.Join(Dir, file), Msg: fmt.Sprintf("name: %q passt nicht zum Dateinamen (%s)", f.Name, name)}
	}
	return f, nil
}

func builtinNames() []string {
	entries, err := fs.ReadDir(examples.Factories, "factories")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && !e.IsDir() {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

func loadBuiltin(name string) (*Factory, error) {
	file := "factories/" + name + ".yaml"
	data, err := fs.ReadFile(examples.Factories, file)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	return Parse("examples/"+file, data, func(rel string) ([]byte, error) {
		return fs.ReadFile(examples.Factories, path.Join("factories", path.Clean(rel)))
	})
}
