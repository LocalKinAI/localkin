package skill

import (
	"encoding/json"
	"fmt"
)

// AskUserName is the kernel-handled question skill.
const AskUserName = "ask_user"

// askUserSkill is a schema-only skill. The kernel intercepts calls to it
// in the turn loop (it needs the turn's context and the front-end's
// prompt channel, neither of which a Skill.Execute has) — the same idea
// as Claude Code's AskUserQuestion: the agent stops and asks a concrete
// question with options instead of guessing.
type askUserSkill struct{}

// NewAskUserSkill returns the stub; souls opt in via skills.enable.
func NewAskUserSkill() Skill { return &askUserSkill{} }

func (s *askUserSkill) Name() string { return AskUserName }
func (s *askUserSkill) Description() string {
	return "Ask the user one question and wait for the answer. Use when you need a decision you cannot make " +
		"yourself: which of several matches, a missing detail (folder, recipient, date), or approval of a " +
		"plan before a long task. Give 2–5 short options when the choice is discrete; the user can always " +
		"type something else. Do NOT use it for tool approvals — the permission gate handles those — and do " +
		"not ask what you could find out with a tool."
}
func (s *askUserSkill) ToolDef() json.RawMessage {
	schema := map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        AskUserName,
			"description": s.Description(),
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"question": map[string]interface{}{
						"type":        "string",
						"description": "The question, in the user's language, one sentence.",
					},
					"options": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "2–5 short choices, optional. Omit for a free-text answer.",
					},
				},
				"required": []string{"question"},
			},
		},
	}
	b, _ := json.Marshal(schema)
	return b
}

func (s *askUserSkill) Execute(map[string]string) (string, error) {
	return "", fmt.Errorf("%s is handled by the kernel, not the registry", AskUserName)
}

// Question is what the front-end shows for an ask_user call.
type Question struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

// ParseQuestion turns ask_user params into a Question.
func ParseQuestion(id string, params map[string]string) Question {
	q := Question{ID: id, Text: params["question"]}
	if raw := params["options"]; raw != "" {
		var opts []string
		if json.Unmarshal([]byte(raw), &opts) == nil {
			for _, o := range opts {
				if o != "" {
					q.Options = append(q.Options, o)
				}
			}
		}
	}
	return q
}
