package proxy

import (
	"bytes"
	"encoding/json"
	"mime"
	"sync"
)

// Usage is the token usage of a request to a provider API, read from the response.
// Like Anthropic, Input only counts tokens without cache; for OpenAI the read cache is
// subtracted from input_tokens.
type Usage struct {
	Model      string `json:"model,omitempty"`
	Input      int64  `json:"input_tokens"`
	Output     int64  `json:"output_tokens"`
	CacheRead  int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite int64  `json:"cache_write_tokens,omitempty"`
}

// How the providers count.
const (
	DialectAnthropic = "anthropic"
	DialectOpenAI    = "openai"
)

// Limits for reading along: larger JSON responses or SSE lines are skipped.
const (
	maxUsageJSON = 16 << 20
	maxUsageLine = 16 << 20
)

// usageScanner reads along the response of a provider API without holding it up and
// collects the token usage. Write is called by the response writer and never fails.
type usageScanner struct {
	dialect string
	sse     bool

	mu    sync.Mutex
	buf   bytes.Buffer
	skip  bool // current line or JSON too large
	usage Usage
	found bool
}

func newUsageScanner(dialect, contentType string) *usageScanner {
	mt, _, _ := mime.ParseMediaType(contentType)
	return &usageScanner{dialect: dialect, sse: mt == "text/event-stream"}
}

func (s *usageScanner) Write(p []byte) (int, error) {
	n := len(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sse {
		if !s.skip {
			if s.buf.Len()+len(p) > maxUsageJSON {
				s.skip = true
				s.buf.Reset()
			} else {
				s.buf.Write(p)
			}
		}
		return n, nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			break
		}
		s.appendLine(p[:i])
		s.endLine()
		p = p[i+1:]
	}
	return n, nil
}

func (s *usageScanner) appendLine(p []byte) {
	if s.skip {
		return
	}
	if s.buf.Len()+len(p) > maxUsageLine {
		s.skip = true
		s.buf.Reset()
		return
	}
	s.buf.Write(p)
}

func (s *usageScanner) endLine() {
	line := bytes.TrimRight(s.buf.Bytes(), "\r")
	if !s.skip {
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			s.event(bytes.TrimSpace(data))
		}
	}
	s.skip = false
	s.buf.Reset()
}

// Result returns the collected usage (nil: none found).
func (s *usageScanner) Result() *Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sse && !s.skip && s.buf.Len() > 0 {
		s.event(s.buf.Bytes())
		s.buf.Reset()
	} else if s.sse && s.buf.Len() > 0 {
		s.endLine()
	}
	if !s.found {
		return nil
	}
	u := s.usage
	return &u
}

// rawUsage combines the providers' fields.
type rawUsage struct {
	// Anthropic
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	// OpenAI Responses
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	// OpenAI Chat Completions
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type usageEnvelope struct {
	Type    string    `json:"type"`
	Model   string    `json:"model"`
	Usage   *rawUsage `json:"usage"`
	Message *struct {
		Model string    `json:"model"`
		Usage *rawUsage `json:"usage"`
	} `json:"message"`
	Response *struct {
		Model string    `json:"model"`
		Usage *rawUsage `json:"usage"`
	} `json:"response"`
}

// event evaluates a JSON object (the whole response or an SSE event).
func (s *usageScanner) event(data []byte) {
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var e usageEnvelope
	if json.Unmarshal(data, &e) != nil {
		return
	}
	switch {
	case e.Message != nil && e.Message.Usage != nil: // Anthropic message_start
		s.apply(e.Message.Model, e.Message.Usage)
	case e.Response != nil && e.Response.Usage != nil: // OpenAI response.completed and others
		s.apply(e.Response.Model, e.Response.Usage)
	case e.Usage != nil: // whole response, Anthropic message_delta, chat chunk with usage
		s.apply(e.Model, e.Usage)
	}
}

// apply takes over the numbers. Anthropic reports the input in message_start and cumulative
// values in message_delta; set fields therefore override earlier ones.
func (s *usageScanner) apply(model string, u *rawUsage) {
	if model != "" {
		s.usage.Model = model
	}
	set := func(dst *int64, v *int64) {
		if v != nil && (*v > 0 || *dst == 0) {
			*dst = *v
		}
	}
	switch {
	case u.PromptTokens != nil || u.CompletionTokens != nil:
		var cached int64
		if u.PromptTokensDetails != nil {
			cached = u.PromptTokensDetails.CachedTokens
		}
		if u.PromptTokens != nil {
			s.usage.Input = max(*u.PromptTokens-cached, 0)
			s.usage.CacheRead = cached
		}
		set(&s.usage.Output, u.CompletionTokens)
	case s.dialect == DialectOpenAI || u.InputTokensDetails != nil:
		var cached int64
		if u.InputTokensDetails != nil {
			cached = u.InputTokensDetails.CachedTokens
		}
		if u.InputTokens != nil {
			s.usage.Input = max(*u.InputTokens-cached, 0)
			s.usage.CacheRead = cached
		}
		set(&s.usage.Output, u.OutputTokens)
	default:
		set(&s.usage.Input, u.InputTokens)
		set(&s.usage.Output, u.OutputTokens)
		set(&s.usage.CacheRead, u.CacheReadInputTokens)
		set(&s.usage.CacheWrite, u.CacheCreationInputTokens)
	}
	s.found = true
}
