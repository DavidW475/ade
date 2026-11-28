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

// Origin of a factory.
const (
	SourceProject = "project" // .ade/factories in the repository
	SourceBuiltin = "builtin" // template from examples/factories
)

// Summary describes an available factory.
type Summary struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Source      string           `json:"source"`
	Path        string           `json:"path"`
	Inputs      map[string]Input `json:"inputs,omitempty"`
	Steps       []StepSummary    `json:"steps,omitempty"`
	// Error is set if the file is invalid.
	Error string `json:"error,omitempty"`
}

// StepSummary is a step for the overview.
type StepSummary struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Role     string `json:"role,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
	// Group is, for members of a parallel group, the position of the first member.
	Group *int `json:"group,omitempty"`
}

// Steps returns the steps (flat) for the overview.
func (f *Factory) Steps() []StepSummary {
	flat := f.Flat()
	out := make([]StepSummary, len(flat))
	for i, s := range flat {
		out[i] = StepSummary{Name: s.Step.Step, Kind: s.Kind(), Role: s.Role}
		if r := f.Roles[s.Role]; r != nil {
			out[i].ReadOnly = r.ReadOnly()
		}
		if s.Group >= 0 {
			out[i].Group = &s.Group
		}
	}
	return out
}

// Load loads the factory name: first from .ade/factories/<name>.yaml in repo, otherwise the
// built-in template. If both are missing, the error is fs.ErrNotExist.
func Load(repo, name string) (*Factory, string, error) {
	if !ValidName(name) {
		return nil, "", fmt.Errorf("%q is not a valid factory name", name)
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

// List returns all factories of a repository (own ones before templates of the same name), by name.
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

// loadProject reads a factory from the repository. Prompts must be under .ade/factories;
// os.Root prevents symlinks from leading out.
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
			return nil, fmt.Errorf("%s is missing", path.Join(Dir, rel))
		}
		return b, err
	})
	if err != nil {
		return nil, err
	}
	if f.Name != name {
		return nil, &Error{Path: path.Join(Dir, file), Msg: fmt.Sprintf("name: %q does not match the file name (%s)", f.Name, name)}
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
