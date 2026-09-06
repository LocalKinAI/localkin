// Package hooks runs user-supplied shell commands at fixed points in
// the agent loop — the same idea as Claude Code's PreToolUse /
// PostToolUse / Stop hooks.
//
// Hooks are how a user gets deterministic behaviour out of a
// probabilistic agent without touching the kernel: "never let `input`
// fire while Zoom is frontmost", "log every shell command to a file",
// "play a sound when a turn ends", "reject any file_write outside my
// project". The soul declares them; the kernel runs them; exit codes
// carry the decision.
//
//	hooks:
//	  pre_tool:
//	    - match: "input"           # skill name, or prefix*; empty = every skill
//	      run: "./hooks/no-input-during-meetings.sh"
//	      timeout: 5               # seconds, default 10
//	  post_tool:
//	    - match: "shell"
//	      run: "tee -a ~/.kinclaw/shell.log >/dev/null"
//	  stop:
//	    - run: "afplay /System/Library/Sounds/Glass.aiff"
//
// The hook receives a JSON payload on stdin. Exit 0 means continue.
// Exit 2 means block — for pre_tool the call is not executed and the
// hook's stderr becomes the tool result the model sees; for post_tool
// the stderr is appended to the tool output as feedback. Any other
// exit code is logged and ignored, so a broken hook degrades the
// agent rather than disabling it.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Hook is one entry under hooks.<event>.
type Hook struct {
	// Match is a skill name or `prefix*`. Empty matches every skill.
	// Ignored for stop hooks.
	Match string `yaml:"match"`
	// Run is a shell command line, executed with the soul's directory
	// as cwd so relative script paths resolve next to the soul.
	Run string `yaml:"run"`
	// Timeout in seconds; default 10. A hook that hangs must not hang
	// the agent.
	Timeout int `yaml:"timeout"`
}

// Config is the `hooks:` block of a soul.
type Config struct {
	PreTool  []Hook `yaml:"pre_tool"`
	PostTool []Hook `yaml:"post_tool"`
	Stop     []Hook `yaml:"stop"`
}

// Empty reports whether no hooks are configured.
func (c Config) Empty() bool {
	return len(c.PreTool) == 0 && len(c.PostTool) == 0 && len(c.Stop) == 0
}

// Payload is what a hook reads from stdin.
type Payload struct {
	Event   string            `json:"event"` // pre_tool | post_tool | stop
	Session string            `json:"session"`
	Skill   string            `json:"skill,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	Output  string            `json:"output,omitempty"` // post_tool: the tool result
	Reply   string            `json:"reply,omitempty"`  // stop: the assistant's final text
	Cwd     string            `json:"cwd"`
}

// Outcome is a hook run's effect on the loop.
type Outcome struct {
	// Blocked is true when a hook exited 2.
	Blocked bool
	// Message is the blocking hook's stderr (trimmed), model-facing.
	Message string
}

// Runner executes a Config's hooks.
type Runner struct {
	cfg     Config
	dir     string
	env     []string
	session string
	// Log receives one line per non-zero, non-blocking exit. Defaults
	// to stderr.
	Log func(string)
}

// New builds a runner. dir is where relative `run` paths resolve
// (the soul's directory); env is the environment for hook processes.
func New(cfg Config, dir, session string, env []string) *Runner {
	if dir == "" {
		dir = "."
	}
	return &Runner{
		cfg: cfg, dir: dir, session: session, env: env,
		Log: func(s string) { fmt.Fprintln(os.Stderr, s) },
	}
}

// Empty reports whether the runner has nothing to run.
func (r *Runner) Empty() bool { return r == nil || r.cfg.Empty() }

// PreTool runs the matching pre_tool hooks in order. The first block wins.
func (r *Runner) PreTool(ctx context.Context, skill string, params map[string]string) Outcome {
	if r.Empty() {
		return Outcome{}
	}
	return r.runMatching(ctx, r.cfg.PreTool, Payload{
		Event: "pre_tool", Skill: skill, Params: params,
	})
}

// PostTool runs the matching post_tool hooks. A block here does not
// undo the call (it already ran); the message is feedback for the model.
func (r *Runner) PostTool(ctx context.Context, skill string, params map[string]string, output string) Outcome {
	if r.Empty() {
		return Outcome{}
	}
	return r.runMatching(ctx, r.cfg.PostTool, Payload{
		Event: "post_tool", Skill: skill, Params: params, Output: output,
	})
}

// Stop runs the stop hooks after a turn produces its final reply.
// Fire-and-forget semantics: outcomes are logged, never returned.
func (r *Runner) Stop(ctx context.Context, reply string) {
	if r.Empty() {
		return
	}
	for _, h := range r.cfg.Stop {
		if out, err := r.run(ctx, h, Payload{Event: "stop", Reply: reply}); err != nil {
			r.Log(fmt.Sprintf("[hooks] stop hook %q: %v %s", h.Run, err, out.Message))
		}
	}
}

func (r *Runner) runMatching(ctx context.Context, hooks []Hook, p Payload) Outcome {
	for _, h := range hooks {
		if !matches(h.Match, p.Skill) {
			continue
		}
		out, err := r.run(ctx, h, p)
		if err != nil {
			r.Log(fmt.Sprintf("[hooks] %s hook %q: %v", p.Event, h.Run, err))
			continue
		}
		if out.Blocked {
			return out
		}
	}
	return Outcome{}
}

// run executes one hook. Returns (Outcome, nil) for exit 0 and exit 2;
// an error for anything else (spawn failure, timeout, other exit codes).
func (r *Runner) run(ctx context.Context, h Hook, p Payload) (Outcome, error) {
	if strings.TrimSpace(h.Run) == "" {
		return Outcome{}, fmt.Errorf("empty run")
	}
	timeout := time.Duration(h.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	p.Session = r.session
	p.Cwd, _ = os.Getwd()
	payload, _ := json.Marshal(p)

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", h.Run)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", h.Run)
	}
	cmd.Dir = r.dir
	if abs, err := filepath.Abs(r.dir); err == nil {
		cmd.Dir = abs
	}
	cmd.Env = append(append([]string{}, r.env...),
		"KINCLAW_HOOK_EVENT="+p.Event,
		"KINCLAW_HOOK_SKILL="+p.Skill,
		"KINCLAW_SESSION="+p.Session,
	)
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{} // hooks may print; the model never sees stdout

	err := cmd.Run()
	msg := strings.TrimSpace(stderr.String())
	if err == nil {
		return Outcome{}, nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return Outcome{Message: msg}, fmt.Errorf("timed out after %s", timeout)
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
		if msg == "" {
			msg = fmt.Sprintf("blocked by %s hook (%s)", p.Event, h.Run)
		}
		return Outcome{Blocked: true, Message: msg}, nil
	}
	return Outcome{Message: msg}, err
}

// matches applies the skill-name grammar: exact, `prefix*`, or "" (all).
func matches(pattern, skill string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(skill, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == skill
}
