package proxy

import (
	"bytes"
	"encoding/json"
	"mime"
	"sync"
)

// Usage ist die Token-Nutzung einer Anfrage an eine Anbieter-API, gelesen aus der Antwort.
// Input zählt wie bei Anthropic nur Tokens ohne Cache; bei OpenAI wird der gelesene Cache
// aus input_tokens herausgerechnet.
type Usage struct {
	Model      string `json:"model,omitempty"`
	Input      int64  `json:"input_tokens"`
	Output     int64  `json:"output_tokens"`
	CacheRead  int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite int64  `json:"cache_write_tokens,omitempty"`
}

// Zählweisen der Anbieter.
const (
	DialectAnthropic = "anthropic"
	DialectOpenAI    = "openai"
)

// Grenzen für das Mitlesen: Größere JSON-Antworten oder SSE-Zeilen werden übersprungen.
const (
	maxUsageJSON = 16 << 20
	maxUsageLine = 16 << 20
)

// usageScanner liest die Antwort einer Anbieter-API mit, ohne sie aufzuhalten, und sammelt
// die Token-Nutzung. Write wird vom Antwort-Writer aufgerufen und scheitert nie.
type usageScanner struct {
	dialect string
	sse     bool

	mu    sync.Mutex
	buf   bytes.Buffer
	skip  bool // aktuelle Zeile bzw. JSON zu groß
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

// Result liefert die gesammelte Nutzung (nil: keine gefunden).
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

// rawUsage vereint die Felder der Anbieter.
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

// event wertet ein JSON-Objekt aus (ganze Antwort oder ein SSE-Event).
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
	case e.Response != nil && e.Response.Usage != nil: // OpenAI response.completed u. a.
		s.apply(e.Response.Model, e.Response.Usage)
	case e.Usage != nil: // ganze Antwort, Anthropic message_delta, Chat-Chunk mit usage
		s.apply(e.Model, e.Usage)
	}
}

// apply übernimmt die Zahlen. Anthropic meldet in message_start die Eingabe und in
// message_delta kumulierte Werte; gesetzte Felder überschreiben daher frühere.
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
