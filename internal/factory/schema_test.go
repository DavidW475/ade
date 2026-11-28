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
		`{"verdict":"maybe","comments":[]}`:                    `$.verdict: only "approve", "changes" are allowed`,
		`{"comments":[]}`:                                      "field verdict missing",
		`{"verdict":"approve","comments":[""]}`:                "$.comments[0]: at least 1 characters",
		`{"verdict":"approve","comments":["a","b","c"]}`:       "at most 2 entries",
		`{"verdict":"approve","comments":[],"score":2.5}`:      "expected integer",
		`{"verdict":"approve","comments":[],"score":11}`:       "at most 10",
		`{"verdict":"approve","comments":[],"extra":1}`:        "field extra is not allowed",
		`[1]`: "expected object, found array",
	}
	for doc, want := range cases {
		var v any
		if err := json.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatal(err)
		}
		got := strings.Join(s.Validate(v), "; ")
		if want == "" && got != "" || !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", doc, got, want)
		}
	}
}

func TestCompileSchemaErrors(t *testing.T) {
	for doc, want := range map[string]string{
		`{"type":"thing"}`:                      `"thing" is unknown`,
		`{"pattern":"x"}`:                       "pattern is not supported",
		`{"properties":{"a":{"$ref":"#/x"}}}`:   "schema .a: keyword $ref",
		`{"additionalProperties":{"type":"x"}}`: "only supported as true or false",
		`{"minLength":-1}`:                      "an integer",
		`[]`:                                    "must be an object",
		`{"enum":[]}`:                           "non-empty list",
	} {
		_, err := CompileSchema([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", doc, err, want)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	for text, want := range map[string]string{
		`{"a":1}`: "{\n  \"a\": 1\n}",
		"Here is the result:\n\n```json\n{\"a\": [1, 2]}\n```\n": "{\n  \"a\": [\n    1,\n    2\n  ]\n}",
		"```json\n{\"a\":1}\n```\nand then\n```\n{\"a\":2}\n```": "{\n  \"a\": 2\n}",
		"```\nkein json\n```\n```json\n{\"b\":true}\n```":        "{\n  \"b\": true\n}",
	} {
		got, _, err := ExtractJSON(text)
		if err != nil || string(got) != want {
			t.Errorf("%q: %q %v", text, got, err)
		}
	}
	for _, text := range []string{"", "All good!", `{"a":1} and more`, "```json\n{broken\n```"} {
		if _, _, err := ExtractJSON(text); err == nil {
			t.Errorf("%q: no error", text)
		}
	}
}
