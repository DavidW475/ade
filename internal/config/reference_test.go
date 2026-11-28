package config_test

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/factory"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/sandbox"
)

// TestReferenceCoversConfig checks doc/configuration.md against the Go structs: every YAML
// field appears in the table of its file, and every table row names an existing field.
func TestReferenceCoversConfig(t *testing.T) {
	data, err := os.ReadFile("../../doc/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	sections := splitSections(string(data))

	for _, tc := range []struct {
		heading string
		root    any
	}{
		{"## `config.yaml`", config.Config{}},
		{"## `.ade/project.yaml`", project.Config{}},
		{"## `.ade/sandbox.yaml`", sandbox.Config{}},
		{"## Factory (`.ade/factories/<name>.yaml`)", factory.Factory{}},
	} {
		t.Run(tc.heading, func(t *testing.T) {
			body, ok := sections[tc.heading]
			if !ok {
				t.Fatalf("section %q missing in doc/configuration.md", tc.heading)
			}
			want := yamlPaths(reflect.TypeOf(tc.root), "", nil)
			rows := tableKeys(body)
			for _, p := range want {
				if !slices.Contains(rows, p) {
					t.Errorf("field `%s` is not documented", p)
				}
			}
			for _, r := range rows {
				// A row may also describe a parent of documented fields (roles.<name>.outputs).
				parent := slices.ContainsFunc(want, func(p string) bool {
					return strings.HasPrefix(p, r+".") || strings.HasPrefix(p, r+"[]")
				})
				if !slices.Contains(want, r) && !parent {
					t.Errorf("documented key `%s` does not exist", r)
				}
			}
		})
	}
}

func splitSections(doc string) map[string]string {
	out := map[string]string{}
	var cur string
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "## ") {
			cur = line
			continue
		}
		if cur != "" {
			out[cur] += line + "\n"
		}
	}
	return out
}

var rowKeyRE = regexp.MustCompile("^\\| `([^`]+)` \\|")

// tableKeys returns the keys in the first column of the section's tables.
func tableKeys(body string) []string {
	var keys []string
	for _, line := range strings.Split(body, "\n") {
		if m := rowKeyRE.FindStringSubmatch(line); m != nil {
			keys = append(keys, m[1])
		}
	}
	return keys
}

var durationType = reflect.TypeOf(time.Duration(0))

// yamlPaths lists the leaf paths of t: maps become "<name>", slices "[]". A struct that is
// already on the path (flow[].parallel) ends as a leaf.
func yamlPaths(t reflect.Type, prefix string, stack []reflect.Type) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == durationType:
	case t.Kind() == reflect.Map && t.Elem().Kind() != reflect.String:
		return yamlPaths(t.Elem(), prefix+".<name>", stack)
	case t.Kind() == reflect.Slice && elemKind(t) == reflect.Struct:
		return yamlPaths(t.Elem(), prefix+"[]", stack)
	case t.Kind() == reflect.Struct && !slices.Contains(stack, t):
		var out []string
		stack = append(stack, t)
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			out = append(out, yamlPaths(f.Type, p, stack)...)
		}
		return out
	}
	return []string{strings.TrimSuffix(strings.TrimPrefix(prefix, "."), "[]")}
}

func elemKind(t reflect.Type) reflect.Kind {
	e := t.Elem()
	for e.Kind() == reflect.Pointer {
		e = e.Elem()
	}
	return e.Kind()
}
