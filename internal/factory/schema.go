package factory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Schema is a checked subset of JSON Schema for structured artifacts: type, properties,
// required, enum, items, additionalProperties (only true/false), minimum, maximum,
// minLength, maxLength, minItems, maxItems; description and title are ignored.
type Schema struct {
	Types      []string
	Properties map[string]*Schema
	Required   []string
	Enum       []any
	Items      *Schema
	// Closed: additionalProperties: false.
	Closed               bool
	Minimum, Maximum     *float64
	MinLength, MaxLength *int
	MinItems, MaxItems   *int
}

var schemaTypes = []string{"object", "array", "string", "number", "integer", "boolean", "null"}

// maxSchemaDepth limits the nesting of a schema.
const maxSchemaDepth = 32

// CompileSchema checks a schema (as JSON) and returns it compiled.
func CompileSchema(doc []byte) (*Schema, error) {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	return compileSchema(v, "", 0)
}

func compileSchema(v any, at string, depth int) (*Schema, error) {
	where := func() string {
		if at == "" {
			return "schema"
		}
		return "schema " + at
	}
	if depth > maxSchemaDepth {
		return nil, fmt.Errorf("%s: nested too deeply", where())
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be an object", where())
	}
	s := &Schema{}
	for _, k := range sortedKeys(m) {
		val := m[k]
		fail := func(msg string) error { return fmt.Errorf("%s: %s %s", where(), k, msg) }
		switch k {
		case "$schema", "$id", "title", "description", "examples", "default":
		case "type":
			switch t := val.(type) {
			case string:
				s.Types = []string{t}
			case []any:
				for _, e := range t {
					str, ok := e.(string)
					if !ok {
						return nil, fail("must contain strings")
					}
					s.Types = append(s.Types, str)
				}
			default:
				return nil, fail("must be a string or a list")
			}
			for _, t := range s.Types {
				if !slices.Contains(schemaTypes, t) {
					return nil, fail(fmt.Sprintf("%q is unknown (allowed: %s)", t, strings.Join(schemaTypes, ", ")))
				}
			}
		case "properties":
			props, ok := val.(map[string]any)
			if !ok {
				return nil, fail("must be an object")
			}
			s.Properties = map[string]*Schema{}
			for _, name := range sortedKeys(props) {
				sub, err := compileSchema(props[name], at+"."+name, depth+1)
				if err != nil {
					return nil, err
				}
				s.Properties[name] = sub
			}
		case "required":
			list, ok := val.([]any)
			if !ok {
				return nil, fail("must be a list")
			}
			for _, e := range list {
				str, ok := e.(string)
				if !ok {
					return nil, fail("must contain strings")
				}
				s.Required = append(s.Required, str)
			}
		case "enum":
			list, ok := val.([]any)
			if !ok || len(list) == 0 {
				return nil, fail("must be a non-empty list")
			}
			s.Enum = list
		case "items":
			sub, err := compileSchema(val, at+"[]", depth+1)
			if err != nil {
				return nil, err
			}
			s.Items = sub
		case "additionalProperties":
			b, ok := val.(bool)
			if !ok {
				return nil, fail("is only supported as true or false")
			}
			s.Closed = !b
		case "minimum", "maximum":
			f, ok := val.(float64)
			if !ok {
				return nil, fail("must be a number")
			}
			if k == "minimum" {
				s.Minimum = &f
			} else {
				s.Maximum = &f
			}
		case "minLength", "maxLength", "minItems", "maxItems":
			f, ok := val.(float64)
			if !ok || f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
				return nil, fail("must be an integer ≥ 0")
			}
			n := int(f)
			switch k {
			case "minLength":
				s.MinLength = &n
			case "maxLength":
				s.MaxLength = &n
			case "minItems":
				s.MinItems = &n
			case "maxItems":
				s.MaxItems = &n
			}
		default:
			return nil, fmt.Errorf("%s: keyword %s is not supported", where(), k)
		}
	}
	return s, nil
}

// maxSchemaErrors limits the reported errors of a check.
const maxSchemaErrors = 10

// Validate checks a value read with encoding/json and returns the errors (empty: valid).
func (s *Schema) Validate(v any) []string {
	var errs []string
	s.validate(v, "$", &errs)
	return errs
}

func (s *Schema) validate(v any, at string, errs *[]string) {
	add := func(format string, args ...any) {
		if len(*errs) < maxSchemaErrors {
			*errs = append(*errs, at+": "+fmt.Sprintf(format, args...))
		}
	}
	if len(s.Types) > 0 && !slices.ContainsFunc(s.Types, func(t string) bool { return hasType(v, t) }) {
		add("expected %s, found %s", strings.Join(s.Types, " or "), typeOf(v))
		return
	}
	if len(s.Enum) > 0 && !slices.ContainsFunc(s.Enum, func(e any) bool { return jsonEqual(e, v) }) {
		var opts []string
		for _, e := range s.Enum {
			b, _ := json.Marshal(e)
			opts = append(opts, string(b))
		}
		add("only %s are allowed", strings.Join(opts, ", "))
	}
	switch x := v.(type) {
	case map[string]any:
		for _, name := range s.Required {
			if _, ok := x[name]; !ok {
				add("field %s missing", name)
			}
		}
		for _, name := range sortedKeys(x) {
			if sub := s.Properties[name]; sub != nil {
				sub.validate(x[name], at+"."+name, errs)
			} else if s.Closed {
				add("field %s is not allowed", name)
			}
		}
	case []any:
		if s.MinItems != nil && len(x) < *s.MinItems {
			add("at least %d entries", *s.MinItems)
		}
		if s.MaxItems != nil && len(x) > *s.MaxItems {
			add("at most %d entries", *s.MaxItems)
		}
		if s.Items != nil {
			for i, e := range x {
				s.Items.validate(e, at+"["+strconv.Itoa(i)+"]", errs)
			}
		}
	case string:
		n := utf8.RuneCountInString(x)
		if s.MinLength != nil && n < *s.MinLength {
			add("at least %d characters", *s.MinLength)
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			add("at most %d characters", *s.MaxLength)
		}
	case float64:
		if s.Minimum != nil && x < *s.Minimum {
			add("at least %v", *s.Minimum)
		}
		if s.Maximum != nil && x > *s.Maximum {
			add("at most %v", *s.Maximum)
		}
	}
}

func hasType(v any, t string) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	}
	return false
}

func typeOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// ExtractJSON gets the JSON from a role's answer: the whole answer or the last code block
// (```json … ```). It returns the indented JSON and the read value.
func ExtractJSON(text string) ([]byte, any, error) {
	text = strings.TrimSpace(text)
	candidates := []string{text}
	if blocks := codeBlocks(text); len(blocks) > 0 {
		candidates = append(slices.Clone(blocks), text)
		slices.Reverse(candidates[:len(blocks)])
	}
	var firstErr error
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(c))
		err := dec.Decode(&v)
		if err == nil && dec.More() {
			err = errors.New("further text follows the JSON value")
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var out bytes.Buffer
		if err := json.Indent(&out, []byte(c), "", "  "); err != nil {
			return nil, nil, err
		}
		return out.Bytes(), v, nil
	}
	if firstErr == nil {
		firstErr = errors.New("empty answer")
	}
	return nil, nil, fmt.Errorf("the answer contains no valid JSON (%w)", firstErr)
}

// codeBlocks returns the contents of the Markdown code blocks in text (```lang … ```).
func codeBlocks(text string) []string {
	var out []string
	lines := strings.Split(text, "\n")
	in := false
	var cur []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			if in {
				out = append(out, strings.Join(cur, "\n"))
				cur, in = nil, false
			} else {
				in = true
			}
			continue
		}
		if in {
			cur = append(cur, l)
		}
	}
	return out
}
