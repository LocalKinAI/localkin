package brain

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// retryDo retries 429 / 5xx responses with exponential backoff (2, 4,
// 8, 16s — ~30s total), honouring Retry-After when the server sends
// one, and stops early if the request's context is cancelled. Cloud
// providers rate-limit in bursts; three attempts one second apart
// (the old policy) gave up before the burst was over, and an
// interactive agent would rather wait half a minute than fail a turn.
func retryDo(client *http.Client, newReq func() (*http.Request, error)) (*http.Response, error) {
	const maxAttempts = 5
	var resp *http.Response
	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err = client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 429 && resp.StatusCode < 500 {
			return resp, nil
		}
		if attempt == maxAttempts-1 {
			return resp, nil
		}
		wait := time.Duration(2<<attempt) * time.Second
		if ra, ok := retryAfter(resp); ok {
			wait = ra
		}
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		resp.Body.Close()
		select {
		case <-time.After(wait):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return resp, nil
}

// retryAfter parses a Retry-After header given in seconds ("0" is a
// valid answer: retry now). Dates are ignored (rare from LLM providers)
// and fall back to the backoff.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Images: paths to image files (PNG/JPEG/GIF/WebP) that should be
	// attached to this message when sent to a vision-capable brain.
	// The brain adapter base64-encodes the file and inlines it as an
	// image_url content block (OpenAI) or image source block (Claude).
	// Skills opt in by including `image://<path>` markers in their text
	// output; the registry's ExecuteToolCalls strips the markers and
	// populates the ToolResult.Images list, which chatLoop then copies
	// into brain.Message.Images. Brains that don't support vision can
	// safely ignore the field — the model just doesn't see the pixels.
	Images []string `json:"-"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ParseArguments flattens the model's tool args (arbitrary JSON
// shape) into the map[string]string skills consume. Primitives
// stringify directly; complex values (arrays, maps, nested objects)
// JSON-encode so skills can parse them back. The previous
// `fmt.Sprintf("%v", v)` path produced Go's default map formatting
// like "map[activeForm:y content:x]" which isn't valid JSON and
// can't be parsed back into structured data — that blocked any
// skill needing array/object parameters (todo_write being the
// motivating case).
//
// Mirrors kincode's stringifyArgs by design — desktop shells +
// kernel agree on the wire format, so SSE event params decode
// uniformly across kinclaw and kincode.
func (tc ToolCall) ParseArguments() (map[string]string, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch s := v.(type) {
		case string:
			out[k] = s
		case nil:
			out[k] = ""
		case bool, int, int64, float64:
			out[k] = fmt.Sprint(s)
		default:
			if blob, err := json.Marshal(s); err == nil {
				out[k] = string(blob)
			} else {
				out[k] = fmt.Sprint(s)
			}
		}
	}
	return out, nil
}

// Usage is the provider-reported token accounting for one Chat call.
// InputTokens is the whole prompt the model actually saw (system +
// tools + every message), which makes it the one honest measure of
// how full the context window is — the kernel's compaction trigger
// reads it in preference to any client-side estimate. Cache fields
// are Anthropic-only (prompt caching); zero elsewhere.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Total is the prompt size including cached tokens — cache reads are
// still tokens in the window, they are just billed differently.
func (u Usage) Total() int { return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens }

type ChatResult struct {
	Content   string
	ToolCalls []ToolCall
	// Usage is zero when the provider didn't report it (some
	// OpenAI-compatible servers omit it on streams).
	Usage Usage
}

type StreamFunc func(chunk string, thinking bool) error
type Brain interface {
	Chat(ctx context.Context, messages []Message, tools []json.RawMessage, onChunk StreamFunc) (*ChatResult, error)
}

type claudeBrain struct {
	baseURL, model, apiKey string
	temperature            float64
	maxTokens              int
	client                 *http.Client
}

func NewClaudeBrain(endpoint, model, apiKey string, temperature float64) Brain {
	if endpoint == "" {
		endpoint = "https://api.anthropic.com"
	}
	return &claudeBrain{
		baseURL: strings.TrimRight(endpoint, "/"), model: model, apiKey: apiKey,
		temperature: temperature, maxTokens: 4096,
		client: &http.Client{Timeout: 5 * time.Minute},
	}
}

type cReq struct {
	Model       string  `json:"model"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature,omitempty"`
	// System is a plain string normally, or an array of text blocks
	// when prompt caching is on (the cache_control marker lives on a
	// block, not on the string form).
	System   interface{}       `json:"system,omitempty"`
	Messages []cMsg            `json:"messages"`
	Stream   bool              `json:"stream"`
	Tools    []json.RawMessage `json:"tools,omitempty"`
}

// cUsage mirrors the `usage` object on both the non-streaming
// response and the message_start / message_delta stream events.
type cUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u *cUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// cacheControl is Anthropic's prompt-caching marker. Put on the system
// block and the last tool definition, it lets the API reuse the
// (large, stable) prefix across the dozens of calls one agent turn
// makes — the soul + skills are identical from round to round, only
// the messages tail grows. Same trick Claude Code relies on.
var cacheControl = map[string]string{"type": "ephemeral"}

type cMsg struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}
type cBlock struct {
	Type      string      `json:"type"`
	Text      string      `json:"text,omitempty"`
	ID        string      `json:"id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Input     interface{} `json:"input,omitempty"`
	ToolUseID string      `json:"tool_use_id,omitempty"`
	// `content` on a tool_result block accepts either a string (legacy
	// text-only) or an array of blocks (multimodal: text + image).
	// Type is interface{} so we can switch based on whether images are
	// attached.
	Content interface{}   `json:"content,omitempty"`
	Source  *cImageSource `json:"source,omitempty"`
}

// cImageSource is the body of a Claude `image` block. Currently only
// base64 inline is supported; URL-based image references are an
// Anthropic API option but require the URL to be reachable from
// Anthropic's servers, which doesn't apply to local screenshots.
type cImageSource struct {
	Type      string `json:"type"`       // always "base64"
	MediaType string `json:"media_type"` // image/png · image/jpeg · etc.
	Data      string `json:"data"`       // raw base64, no `data:` prefix
}
type cResp struct {
	Content []cBlock `json:"content"`
	Usage   *cUsage  `json:"usage,omitempty"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}
type cStreamEvent struct {
	Type         string  `json:"type"`
	Index        int     `json:"index,omitempty"`
	Delta        *cDelta `json:"delta,omitempty"`
	ContentBlock *cBlock `json:"content_block,omitempty"`
	// message_start carries the prompt-side usage (input + cache);
	// message_delta carries the cumulative output_tokens.
	Message *struct {
		Usage *cUsage `json:"usage,omitempty"`
	} `json:"message,omitempty"`
	Usage *cUsage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
type cDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

func (b *claudeBrain) Chat(ctx context.Context, messages []Message, tools []json.RawMessage, onChunk StreamFunc) (*ChatResult, error) {
	var systemPrompt string
	var convMsgs []cMsg
	for _, m := range messages {
		switch {
		case m.Role == RoleSystem:
			systemPrompt = m.Content
		case m.Role == RoleAssistant && len(m.ToolCalls) > 0:
			var blocks []cBlock
			if m.Content != "" {
				blocks = append(blocks, cBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input interface{}
				json.Unmarshal([]byte(tc.Function.Arguments), &input)
				blocks = append(blocks, cBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input,
				})
			}
			convMsgs = append(convMsgs, cMsg{Role: "assistant", Content: blocks})
		case m.Role == RoleTool:
			// Build the tool_result block. If the message has attached
			// images, content becomes an array of [text, image, image, …]
			// instead of the bare string Claude accepts when there's no
			// multimedia. Vision-capable models (Sonnet/Opus 3.5+, Haiku
			// 3.5+) read the images alongside the text result.
			var toolResultContent interface{}
			if len(m.Images) > 0 {
				inner := make([]cBlock, 0, 1+len(m.Images))
				if m.Content != "" {
					inner = append(inner, cBlock{Type: "text", Text: m.Content})
				}
				for _, p := range m.Images {
					mt, b64, err := imageToBase64(p)
					if err != nil {
						continue // skip unreadable / unsupported
					}
					inner = append(inner, cBlock{
						Type:   "image",
						Source: &cImageSource{Type: "base64", MediaType: mt, Data: b64},
					})
				}
				toolResultContent = inner
			} else {
				toolResultContent = m.Content
			}
			convMsgs = append(convMsgs, cMsg{
				Role: "user",
				Content: []cBlock{{
					Type: "tool_result", ToolUseID: m.ToolCallID, Content: toolResultContent,
				}},
			})
		default:
			convMsgs = append(convMsgs, cMsg{Role: m.Role, Content: m.Content})
		}
	}
	claudeTools := convertToolsToClaude(tools)
	// Prompt caching only against Anthropic itself. Anthropic-format
	// proxies and third-party endpoints don't all accept cache_control,
	// and a 400 on every call is a worse failure than a cache miss.
	var system interface{} = systemPrompt
	if b.promptCaching() {
		if systemPrompt != "" {
			system = []map[string]interface{}{{
				"type": "text", "text": systemPrompt, "cache_control": cacheControl,
			}}
		}
		claudeTools = markLastToolCacheable(claudeTools)
	}
	if systemPrompt == "" {
		system = nil
	}
	reqBody := cReq{
		Model: b.model, MaxTokens: b.maxTokens, Temperature: b.temperature,
		System: system, Messages: convMsgs, Stream: onChunk != nil, Tools: claudeTools,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	resp, err := retryDo(b.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
		if strings.HasPrefix(b.apiKey, "sk-ant-oat") {
			req.Header.Set("Authorization", "Bearer "+b.apiKey)
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		} else {
			req.Header.Set("x-api-key", b.apiKey)
		}
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("Claude API request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var errResp cResp
		json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Error != nil {
			return nil, fmt.Errorf("Claude API error (%d): %s", resp.StatusCode, errResp.Error.Message)
		}
		return nil, fmt.Errorf("Claude API returned status %d", resp.StatusCode)
	}
	if onChunk == nil {
		var chatResp cResp
		if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
			return nil, fmt.Errorf("decoding response: %w", err)
		}
		result := &ChatResult{Usage: chatResp.Usage.toUsage()}
		var textParts []string
		for _, block := range chatResp.Content {
			switch block.Type {
			case "text":
				textParts = append(textParts, block.Text)
			case "tool_use":
				args, _ := json.Marshal(block.Input)
				result.ToolCalls = append(result.ToolCalls, ToolCall{
					ID: block.ID, Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: block.Name, Arguments: string(args)},
				})
			}
		}
		result.Content = strings.Join(textParts, "")
		return result, nil
	}
	var full strings.Builder
	var streamToolCalls []ToolCall
	var usage Usage
	type toolBlock struct {
		id, name  string
		inputJSON strings.Builder
	}
	activeTools := make(map[int]*toolBlock)
	scanner := bufio.NewScanner(resp.Body)
	// Tool inputs and long text deltas can exceed bufio's 64KB default
	// line limit; a single oversized SSE line would otherwise end the
	// scan early and silently truncate the reply.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var event cStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if event.Error != nil {
			return &ChatResult{Content: full.String(), Usage: usage}, fmt.Errorf("Claude stream error: %s", event.Error.Message)
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil && event.Message.Usage != nil {
				u := event.Message.Usage.toUsage()
				usage.InputTokens, usage.CacheReadTokens, usage.CacheWriteTokens =
					u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens
			}
		case "message_delta":
			if event.Usage != nil {
				usage.OutputTokens = event.Usage.OutputTokens
			}
		case "content_block_start":
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				activeTools[event.Index] = &toolBlock{
					id: event.ContentBlock.ID, name: event.ContentBlock.Name,
				}
			}
		case "content_block_delta":
			if event.Delta == nil {
				continue
			}
			switch event.Delta.Type {
			case "text_delta":
				if event.Delta.Text != "" {
					full.WriteString(event.Delta.Text)
					if err := onChunk(event.Delta.Text, false); err != nil {
						return &ChatResult{Content: full.String()}, err
					}
				}
			case "input_json_delta":
				if tb, ok := activeTools[event.Index]; ok {
					tb.inputJSON.WriteString(event.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if tb, ok := activeTools[event.Index]; ok {
				args := tb.inputJSON.String()
				if args == "" {
					args = "{}"
				}
				streamToolCalls = append(streamToolCalls, ToolCall{
					ID: tb.id, Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: tb.name, Arguments: args},
				})
				delete(activeTools, event.Index)
			}
		}
	}
	return &ChatResult{Content: full.String(), ToolCalls: streamToolCalls, Usage: usage}, nil
}

// promptCaching reports whether this brain talks to Anthropic directly,
// the only endpoint where cache_control markers are known-safe.
func (b *claudeBrain) promptCaching() bool {
	return strings.Contains(b.baseURL, "anthropic.com")
}

// markLastToolCacheable adds cache_control to the final tool definition
// so the whole tools array (a stable, often multi-thousand-token block)
// becomes one cache prefix together with the system prompt.
func markLastToolCacheable(tools []json.RawMessage) []json.RawMessage {
	if len(tools) == 0 {
		return tools
	}
	var last map[string]interface{}
	if err := json.Unmarshal(tools[len(tools)-1], &last); err != nil {
		return tools
	}
	last["cache_control"] = cacheControl
	b, err := json.Marshal(last)
	if err != nil {
		return tools
	}
	out := make([]json.RawMessage, len(tools))
	copy(out, tools)
	out[len(out)-1] = b
	return out
}

func convertToolsToClaude(tools []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(tools))
	for _, raw := range tools {
		var openai struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &openai); err != nil || openai.Type != "function" {
			out = append(out, raw)
			continue
		}
		claude := struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}{
			Type: "custom", Name: openai.Function.Name,
			Description: openai.Function.Description, InputSchema: openai.Function.Parameters,
		}
		b, _ := json.Marshal(claude)
		out = append(out, b)
	}
	return out
}

type openAIBrain struct {
	baseURL, model, apiKey string
	temperature            float64
	maxTokens              int
	client                 *http.Client
}

func NewOpenAIBrain(endpoint, model, apiKey string, temperature float64) Brain {
	if endpoint == "" {
		endpoint = "https://api.openai.com"
	}
	return &openAIBrain{
		baseURL: strings.TrimRight(endpoint, "/"), model: model, apiKey: apiKey,
		temperature: temperature, maxTokens: 4096,
		client: &http.Client{Timeout: 5 * time.Minute},
	}
}

type oaiReq struct {
	Model       string            `json:"model"`
	Messages    []oaiMsg          `json:"messages"`
	Temperature float64           `json:"temperature,omitempty"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Stream      bool              `json:"stream"`
	Tools       []json.RawMessage `json:"tools,omitempty"`
	// StreamOptions asks for a final `usage` chunk on streams. OpenAI,
	// Ollama, Groq and DeepSeek all honour it; without it a streamed
	// reply carries no token counts at all.
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u *oaiUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
}

type oaiMsg struct {
	Role string `json:"role"`
	// `content` must always serialize, even as "". OpenAI's own endpoint
	// tolerates a missing content field for assistant messages with
	// tool_calls, but Ollama Cloud / Kimi K2.x's OpenAI adapter strictly
	// requires the field to exist — missing → "invalid message content
	// type: <nil>" HTTP 400. So: no `omitempty`.
	//
	// Type is `interface{}` because the OpenAI vision API accepts content
	// as either a plain string OR an array of content blocks. We send a
	// string when there are no images attached (preserves the existing
	// wire format for text-only flows) and an array of `oaiContentBlock`
	// when images are attached (text + image_url blocks).
	Content    interface{} `json:"content"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

// oaiContentBlock is a single element of a multimodal content array.
// type=text uses Text; type=image_url uses ImageURL.
type oaiContentBlock struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL string `json:"url"` // data:image/png;base64,...
}
type oaiResp struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (b *openAIBrain) Chat(ctx context.Context, messages []Message, tools []json.RawMessage, onChunk StreamFunc) (*ChatResult, error) {
	var oaiMsgs []oaiMsg
	for _, m := range messages {
		msg := oaiMsg{Role: m.Role, ToolCallID: m.ToolCallID}
		if len(m.ToolCalls) > 0 {
			msg.ToolCalls = m.ToolCalls
		}
		// If the message has attached images, build a multimodal content
		// array. Otherwise keep the simple string content for backward
		// compatibility with strict OpenAI-compat servers (Ollama Cloud,
		// Groq, etc. that may not all accept array content for every role).
		if len(m.Images) > 0 {
			blocks := make([]oaiContentBlock, 0, 1+len(m.Images))
			if m.Content != "" {
				blocks = append(blocks, oaiContentBlock{Type: "text", Text: m.Content})
			}
			for _, p := range m.Images {
				url, err := imageToDataURL(p)
				if err != nil {
					// Don't fail the whole request — just skip the
					// unreadable / unsupported image and continue.
					// The text content still describes what should be
					// attached, so the model has SOME context.
					continue
				}
				blocks = append(blocks, oaiContentBlock{
					Type:     "image_url",
					ImageURL: &oaiImageURL{URL: url},
				})
			}
			msg.Content = blocks
		} else {
			msg.Content = m.Content
		}
		oaiMsgs = append(oaiMsgs, msg)
	}
	reqBody := oaiReq{
		Model: b.model, Messages: oaiMsgs, Temperature: b.temperature,
		MaxTokens: b.maxTokens, Stream: onChunk != nil, Tools: tools,
	}
	if onChunk != nil {
		reqBody.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	resp, err := retryDo(b.client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("OpenAI API request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var errResp oaiResp
		json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Error != nil {
			return nil, fmt.Errorf("OpenAI API error (%d): %s", resp.StatusCode, errResp.Error.Message)
		}
		return nil, fmt.Errorf("OpenAI API returned status %d", resp.StatusCode)
	}
	if onChunk == nil {
		var chatResp oaiResp
		if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
			return nil, fmt.Errorf("decoding response: %w", err)
		}
		if len(chatResp.Choices) == 0 {
			return &ChatResult{Usage: chatResp.Usage.toUsage()}, nil
		}
		choice := chatResp.Choices[0].Message
		return &ChatResult{Content: choice.Content, ToolCalls: choice.ToolCalls, Usage: chatResp.Usage.toUsage()}, nil
	}
	var full strings.Builder
	var usage Usage
	toolCallMap := make(map[int]*ToolCall)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var event oaiResp
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if event.Usage != nil {
			usage = event.Usage.toUsage()
		}
		if len(event.Choices) == 0 {
			// The usage-only trailer chunk has an empty choices array.
			continue
		}
		delta := event.Choices[0].Delta
		if delta.Content != "" {
			full.WriteString(delta.Content)
			if err := onChunk(delta.Content, false); err != nil {
				return &ChatResult{Content: full.String()}, err
			}
		}
		for _, tc := range delta.ToolCalls {
			if tc.ID != "" {
				toolCallMap[tc.Index] = &ToolCall{ID: tc.ID, Type: "function"}
				toolCallMap[tc.Index].Function.Name = tc.Function.Name
			}
			if existing, ok := toolCallMap[tc.Index]; ok {
				existing.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	var streamToolCalls []ToolCall
	for i := 0; i < len(toolCallMap); i++ {
		if tc, ok := toolCallMap[i]; ok {
			streamToolCalls = append(streamToolCalls, *tc)
		}
	}
	return &ChatResult{Content: full.String(), ToolCalls: streamToolCalls, Usage: usage}, nil
}

func NewBrain(provider, endpoint, model, apiKey string, temperature float64) Brain {
	switch provider {
	case "openai", "ollama":
		return NewOpenAIBrain(endpoint, model, apiKey, temperature)
	default:
		return NewClaudeBrain(endpoint, model, apiKey, temperature)
	}
}
