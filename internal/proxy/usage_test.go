package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func scan(dialect, contentType string, chunks ...string) *Usage {
	s := newUsageScanner(dialect, contentType)
	for _, c := range chunks {
		_, _ = s.Write([]byte(c))
	}
	return s.Result()
}

func TestUsageScanner(t *testing.T) {
	cases := []struct {
		name    string
		dialect string
		ct      string
		chunks  []string
		want    *Usage
	}{
		{"anthropic json", DialectAnthropic, "application/json", []string{
			`{"id":"m","model":"claude-sonnet-4-5","usage":{"input_tokens":12,"cache_read_input_tokens":100,`, `"cache_creation_input_tokens":7,"output_tokens":34}}`,
		}, &Usage{Model: "claude-sonnet-4-5", Input: 12, Output: 34, CacheRead: 100, CacheWrite: 7}},
		{"anthropic sse", DialectAnthropic, "text/event-stream; charset=utf-8", []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":50,\"cache_creation_input_tokens\":0,\"output_tokens\":1}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hallo\"}}\n\nevent: message_delta\nda",
			"ta: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\r\n\r\n",
		}, &Usage{Model: "claude-opus-5-5", Input: 5, Output: 42, CacheRead: 50}},
		{"openai responses sse", DialectOpenAI, "text/event-stream", []string{
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5.3-codex\"}}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.3-codex\",\"usage\":{\"input_tokens\":1000,\"input_tokens_details\":{\"cached_tokens\":200},\"output_tokens\":50}}}\n\n",
		}, &Usage{Model: "gpt-5.3-codex", Input: 800, Output: 50, CacheRead: 200}},
		{"openai chat json", DialectOpenAI, "application/json", []string{
			`{"model":"gpt-5-mini","usage":{"prompt_tokens":30,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":10}}}`,
		}, &Usage{Model: "gpt-5-mini", Input: 20, Output: 4, CacheRead: 10}},
		{"ohne usage", DialectAnthropic, "application/json", []string{`{"error":"x"}`}, nil},
		{"kein json", DialectAnthropic, "text/plain", []string{`hallo`}, nil},
	}
	for _, tc := range cases {
		got := scan(tc.dialect, tc.ct, tc.chunks...)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestProviderCountsUsage(t *testing.T) {
	var auth, enc string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, enc = r.Header.Get("Authorization"), r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5\",\"usage\":{\"input_tokens\":10,\"output_tokens\":3}}}\n\n")
	}))
	defer upstream.Close()
	up, _ := url.Parse(upstream.URL)
	s, ts := testProxy(t, up.Host, Options{Providers: map[string]Provider{"openai": {Upstream: up, Header: "Authorization", Prefix: "Bearer ", Dialect: DialectOpenAI}}})
	if err := s.SetPolicy(Policy{Secrets: map[string]string{"openai": "sk-echt"}}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/p/openai/v1/responses", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer platzhalter")
	req.Header.Set("Accept-Encoding", "br")
	resp, err := http.DefaultTransport.(*http.Transport).Clone().RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "response.completed") {
		t.Fatalf("Antwort %q", body)
	}
	if auth != "Bearer sk-echt" || enc == "br" {
		t.Errorf("Upstream sah Authorization %q, Accept-Encoding %q", auth, enc)
	}
	ev := lastEvent(t, s)
	if ev.Usage == nil || *ev.Usage != (Usage{Model: "gpt-5", Input: 10, Output: 3}) {
		t.Fatalf("Usage = %+v", ev.Usage)
	}
}
