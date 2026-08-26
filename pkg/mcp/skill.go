package mcp

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/LocalKinAI/kinclaw/pkg/skill"
)

// Skill adapts one MCP server tool to kinclaw's skill.Skill interface, so
// MCP tools register alongside the built-in ones and reach the model through
// exactly the same path.
type Skill struct {
	client *Client
	name   string
	desc   string
	schema map[string]any
}

var _ skill.Skill = (*Skill)(nil)

// NamePrefix marks tools that came from an MCP server.
//
// Prefixing is not cosmetic: server tool names are chosen by third parties
// and collide freely with the 188 built-ins (`read`, `search`, `run` are all
// plausible). A collision would silently shadow a built-in skill, so the
// namespace is kept separate. It also gives soul authors a stable pattern to
// match on when deciding what to enable.
const NamePrefix = "mcp_"

func (s *Skill) Name() string        { return NamePrefix + s.client.Name() + "_" + s.name }
func (s *Skill) Description() string { return s.desc }

// ToolDef emits OpenAI function-calling JSON, the shape kinclaw's brain
// expects. MCP's inputSchema is already JSON Schema, so it drops straight
// into `parameters` — unlike skill.MakeToolDef, which only builds flat
// string properties and would lose the nested schemas MCP tools rely on.
func (s *Skill) ToolDef() json.RawMessage {
	params := s.schema
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	def := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        s.Name(),
			"description": s.desc,
			"parameters":  params,
		},
	}
	raw, err := json.Marshal(def)
	if err != nil {
		// Only reachable if the server sent a schema containing something
		// json.Marshal rejects. Fall back to a no-parameter definition
		// rather than dropping the tool entirely.
		fallback, _ := json.Marshal(map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        s.Name(),
				"description": s.desc,
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
		return fallback
	}
	return raw
}

// Execute converts kinclaw's flattened params back into typed JSON values
// and calls the server.
//
// kinclaw hands skills map[string]string: ToolCall.ParseArguments stringifies
// primitives and JSON-encodes anything structured. That is lossless but it is
// not what MCP wants — a server declaring `{"type":"integer"}` and receiving
// `"5"` will reject it as a type error. So each value is coerced back using
// the tool's own declared schema, which is the only reliable description of
// what it expects.
func (s *Skill) Execute(params map[string]string) (string, error) {
	props, _ := s.schema["properties"].(map[string]any)

	args := make(map[string]any, len(params))
	for k, v := range params {
		var propSchema map[string]any
		if props != nil {
			propSchema, _ = props[k].(map[string]any)
		}
		args[k] = coerce(v, propSchema)
	}
	return s.client.CallTool(s.name, args)
}

// coerce turns one stringified value back into the type its schema declares.
// Unparseable values are passed through as strings: the server's own error
// message is more useful than one invented here.
func coerce(raw string, propSchema map[string]any) any {
	declared, _ := propSchema["type"].(string)

	switch declared {
	case "string":
		return raw
	case "integer":
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	case "number":
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return f
		}
	case "boolean":
		if b, err := strconv.ParseBool(raw); err == nil {
			return b
		}
	case "array", "object":
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			return v
		}
	case "":
		// No declared type — common for `anyOf`/`oneOf` schemas, and for
		// servers that simply omit it. Guess only when the text is
		// unambiguously structured; treating a bare "123" as a number here
		// would corrupt string fields that happen to hold digits, such as
		// zip codes or IDs with leading zeros.
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var v any
			if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
				return v
			}
		}
	}
	return raw
}

// SkillsFromClient wraps every tool the server advertises.
func SkillsFromClient(c *Client) []skill.Skill {
	var out []skill.Skill
	for _, td := range c.Tools() {
		out = append(out, &Skill{
			client: c,
			name:   td.Name,
			desc:   td.Description,
			schema: td.InputSchema,
		})
	}
	return out
}
