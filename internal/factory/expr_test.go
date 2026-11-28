package factory

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExprEval(t *testing.T) {
	data := map[string]string{
		"review": `{"verdict": "changes", "comments": ["a", "b"], "score": 7, "ok": false}`,
		"test":   `{"failed": 0, "suite": {"name": "unit"}}`,
	}
	lookup := func(step string) (any, bool) {
		raw, ok := data[step]
		if !ok {
			return nil, false
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		return v, true
	}
	for src, want := range map[string]bool{
		"review.verdict == 'changes' || test.failed > 0":  true,
		"review.verdict == 'approve' || test.failed > 0":  false,
		`review.verdict != "approve" && test.failed == 0`: true,
		"len(review.comments) == 2":                       true,
		"len(review.comments) > 2":                        false,
		"!review.ok":                                      true,
		"!(review.score >= 7)":                            false,
		"review.score < 10 && review.score > 6.5":         true,
		"test.suite.name == 'unit'":                       true,
		"review.missing == null":                          true,
		"review.missing > 0":                              false,
		"plan.x == null":                                  true,
		"review.verdict > 3":                              false,
		"review.comments":                                 true,
		"test.failed":                                     false,
		"true && (false || review.verdict == 'changes')":  true,
	} {
		e, err := ParseExpr(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got := e.Eval(lookup); got != want {
			t.Errorf("%s: %v, want %v", src, got, want)
		}
	}
}

func TestExprErrors(t *testing.T) {
	for src, want := range map[string]string{
		"":                               "ends unexpectedly",
		"review.verdict = 'x'":           "write comparisons with ==",
		"review.verdict == 'x":           "is not closed",
		"review == 'x'":                  "is not a path",
		"(review.a == 1":                 ") missing",
		"review.a == 1 == true":          "do not chain",
		"review.a == 1 review.b":         "unexpected",
		"review. == 1":                   "field name",
		"review.a # 1":                   "unexpected character",
		strings.Repeat("!", 100) + "x.y": "too deeply",
	} {
		_, err := ParseExpr(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", src, err, want)
		}
	}
	e, err := ParseExpr("a.b == 1 && len(c.d.e) > 0")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Paths(); len(got) != 2 || strings.Join(got[1], ".") != "c.d.e" {
		t.Fatalf("Pfade %v", got)
	}
}
