package factory

import (
	"encoding/json"
	"strings"
	"testing"
)

const reviewSchema = `{
  "type": "object",
  "required": ["verdict", "comments"],
  "additionalProperties": false,
  "properties": {
    "verdict": { "enum": ["approve", "changes"] },
    "comments": { "type": "array", "maxItems": 2, "items": { "type": "string", "minLength": 1 } },
    "score": { "type": "integer", "minimum": 0, "maximum": 10 }
  }
}`

func TestSchemaValidate(t *testing.T) {
	s, err := CompileSchema([]byte(reviewSchema))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`{"verdict":"approve","comments":[]}`:                  "",
		`{"verdict":"changes","comments":["a","b"],"score":3}`: "",
		`{"verdict":"maybe","comments":[]}`:                    `$.verdict: erlaubt sind nur "approve", "changes"`,
		`{"comments":[]}`:                                      "Feld verdict fehlt",
		`{"verdict":"approve","comments":[""]}`:                "$.comments[0]: mindestens 1 Zeichen",
		`{"verdict":"approve","comments":["a","b","c"]}`:       "höchstens 2 Einträge",
		`{"verdict":"approve","comments":[],"score":2.5}`:      "erwartet integer",
		`{"verdict":"approve","comments":[],"score":11}`:       "höchstens 10",
		`{"verdict":"approve","comments":[],"extra":1}`:        "Feld extra ist nicht erlaubt",
		`[1]`: "erwartet object, gefunden array",
	}
	for doc, want := range cases {
		var v any
		if err := json.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatal(err)
		}
		got := strings.Join(s.Validate(v), "; ")
		if want == "" && got != "" || !strings.Contains(got, want) {
			t.Errorf("%s: %q, erwartet %q", doc, got, want)
		}
	}
}

func TestCompileSchemaErrors(t *testing.T) {
	for doc, want := range map[string]string{
		`{"type":"thing"}`:                      `"thing" ist unbekannt`,
		`{"pattern":"x"}`:                       "pattern wird nicht unterstützt",
		`{"properties":{"a":{"$ref":"#/x"}}}`:   "Schema .a: Schlüsselwort $ref",
		`{"additionalProperties":{"type":"x"}}`: "nur als true oder false",
		`{"minLength":-1}`:                      "ganze Zahl",
		`[]`:                                    "muss ein Objekt sein",
		`{"enum":[]}`:                           "nicht leere Liste",
	} {
		_, err := CompileSchema([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, erwartet %q", doc, err, want)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	for text, want := range map[string]string{
		`{"a":1}`: "{\n  \"a\": 1\n}",
		"Hier das Ergebnis:\n\n```json\n{\"a\": [1, 2]}\n```\n":  "{\n  \"a\": [\n    1,\n    2\n  ]\n}",
		"```json\n{\"a\":1}\n```\nund dann\n```\n{\"a\":2}\n```": "{\n  \"a\": 2\n}",
		"```\nkein json\n```\n```json\n{\"b\":true}\n```":        "{\n  \"b\": true\n}",
	} {
		got, _, err := ExtractJSON(text)
		if err != nil || string(got) != want {
			t.Errorf("%q: %q %v", text, got, err)
		}
	}
	for _, text := range []string{"", "Alles gut!", `{"a":1} und mehr`, "```json\n{kaputt\n```"} {
		if _, _, err := ExtractJSON(text); err == nil {
			t.Errorf("%q: kein Fehler", text)
		}
	}
}
