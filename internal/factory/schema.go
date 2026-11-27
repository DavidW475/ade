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

// Schema ist eine geprüfte Teilmenge von JSON Schema für strukturierte Artefakte:
// type, properties, required, enum, items, additionalProperties (nur true/false), minimum,
// maximum, minLength, maxLength, minItems, maxItems; description und title werden ignoriert.
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

// maxSchemaDepth begrenzt die Verschachtelung eines Schemas.
const maxSchemaDepth = 32

// CompileSchema prüft ein Schema (als JSON) und liefert es übersetzt.
func CompileSchema(doc []byte) (*Schema, error) {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, fmt.Errorf("kein gültiges JSON: %w", err)
	}
	return compileSchema(v, "", 0)
}

func compileSchema(v any, at string, depth int) (*Schema, error) {
	where := func() string {
		if at == "" {
			return "Schema"
		}
		return "Schema " + at
	}
	if depth > maxSchemaDepth {
		return nil, fmt.Errorf("%s: zu tief verschachtelt", where())
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: muss ein Objekt sein", where())
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
						return nil, fail("muss Zeichenketten enthalten")
					}
					s.Types = append(s.Types, str)
				}
			default:
				return nil, fail("muss eine Zeichenkette oder Liste sein")
			}
			for _, t := range s.Types {
				if !slices.Contains(schemaTypes, t) {
					return nil, fail(fmt.Sprintf("%q ist unbekannt (erlaubt: %s)", t, strings.Join(schemaTypes, ", ")))
				}
			}
		case "properties":
			props, ok := val.(map[string]any)
			if !ok {
				return nil, fail("muss ein Objekt sein")
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
				return nil, fail("muss eine Liste sein")
			}
			for _, e := range list {
				str, ok := e.(string)
				if !ok {
					return nil, fail("muss Zeichenketten enthalten")
				}
				s.Required = append(s.Required, str)
			}
		case "enum":
			list, ok := val.([]any)
			if !ok || len(list) == 0 {
				return nil, fail("muss eine nicht leere Liste sein")
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
				return nil, fail("wird nur als true oder false unterstützt")
			}
			s.Closed = !b
		case "minimum", "maximum":
			f, ok := val.(float64)
			if !ok {
				return nil, fail("muss eine Zahl sein")
			}
			if k == "minimum" {
				s.Minimum = &f
			} else {
				s.Maximum = &f
			}
		case "minLength", "maxLength", "minItems", "maxItems":
			f, ok := val.(float64)
			if !ok || f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
				return nil, fail("muss eine ganze Zahl ≥ 0 sein")
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
			return nil, fmt.Errorf("%s: Schlüsselwort %s wird nicht unterstützt", where(), k)
		}
	}
	return s, nil
}

// maxSchemaErrors begrenzt die gemeldeten Fehler einer Prüfung.
const maxSchemaErrors = 10

// Validate prüft einen mit encoding/json gelesenen Wert und liefert die Fehler (leer: gültig).
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
		add("erwartet %s, gefunden %s", strings.Join(s.Types, " oder "), typeOf(v))
		return
	}
	if len(s.Enum) > 0 && !slices.ContainsFunc(s.Enum, func(e any) bool { return jsonEqual(e, v) }) {
		var opts []string
		for _, e := range s.Enum {
			b, _ := json.Marshal(e)
			opts = append(opts, string(b))
		}
		add("erlaubt sind nur %s", strings.Join(opts, ", "))
	}
	switch x := v.(type) {
	case map[string]any:
		for _, name := range s.Required {
			if _, ok := x[name]; !ok {
				add("Feld %s fehlt", name)
			}
		}
		for _, name := range sortedKeys(x) {
			if sub := s.Properties[name]; sub != nil {
				sub.validate(x[name], at+"."+name, errs)
			} else if s.Closed {
				add("Feld %s ist nicht erlaubt", name)
			}
		}
	case []any:
		if s.MinItems != nil && len(x) < *s.MinItems {
			add("mindestens %d Einträge", *s.MinItems)
		}
		if s.MaxItems != nil && len(x) > *s.MaxItems {
			add("höchstens %d Einträge", *s.MaxItems)
		}
		if s.Items != nil {
			for i, e := range x {
				s.Items.validate(e, at+"["+strconv.Itoa(i)+"]", errs)
			}
		}
	case string:
		n := utf8.RuneCountInString(x)
		if s.MinLength != nil && n < *s.MinLength {
			add("mindestens %d Zeichen", *s.MinLength)
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			add("höchstens %d Zeichen", *s.MaxLength)
		}
	case float64:
		if s.Minimum != nil && x < *s.Minimum {
			add("mindestens %v", *s.Minimum)
		}
		if s.Maximum != nil && x > *s.Maximum {
			add("höchstens %v", *s.Maximum)
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

// ExtractJSON holt das JSON aus der Antwort einer Rolle: die ganze Antwort oder der letzte
// Codeblock (```json … ```). Geliefert wird das eingerückte JSON und der gelesene Wert.
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
			err = errors.New("nach dem JSON-Wert folgt weiterer Text")
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
		firstErr = errors.New("leere Antwort")
	}
	return nil, nil, fmt.Errorf("die Antwort enthält kein gültiges JSON (%w)", firstErr)
}

// codeBlocks liefert den Inhalt der Markdown-Codeblöcke in text (```lang … ```).
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
