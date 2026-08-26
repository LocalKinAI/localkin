// Package mcp implements a Model Context Protocol client, letting kinclaw
// use tools published by any MCP-compatible server instead of only the
// skills built into this repo.
//
// Ported from kincode's internal/mcp, which has been running this protocol
// in production — the wire format and handshake are unchanged so a server
// configured for one works in the other. What is new here are robustness
// fixes that matter more for a long-lived desktop process than for a CLI
// session: stderr is drained to a per-server log file, requests time out,
// and a dead server is reported rather than hanging the agent forever.
//
// Config format is the ecosystem-standard `mcpServers` block — same as
// Claude Desktop, per the MCP docs — so a server's published install snippet
// pastes in unchanged. That block only ever describes local stdio servers;
// remote servers are added through OAuth in the host UI, not this file, so
// there are deliberately no url/headers fields here.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// callTimeout bounds a single JSON-RPC round trip.
//
// Without it a server that accepts a request and never answers wedges the
// agent permanently: the read loop blocks holding the client mutex, so every
// later call queues behind it and the tool never returns. A CLI can be
// ctrl-C'd; a menubar app that has been running for days cannot.
const callTimeout = 60 * time.Second

// Client connects to an MCP server via stdio.
type Client struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex
	nextID atomic.Int64
	tools  []ToolDef

	// Open handle to this server's stderr log, closed with the client so a
	// long session reconnecting servers doesn't leak file descriptors.
	stderrLog *os.File

	closed atomic.Bool
}

// ToolDef represents an MCP tool definition from the server.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    struct{}   `json:"capabilities"`
	ClientInfo      clientInfo `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsListResult struct {
	Tools []ToolDef `json:"tools"`
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type callToolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// connect spawns an MCP server process and performs the initialize
// handshake. Unexported so the package-level Connect is the config-driven
// entry point; ConnectServer exposes this for one-off use.
func connect(name, command string, args []string, env []string) (*Client, error) {
	cmd := exec.Command(command, args...)
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %s: stdin pipe: %w", name, err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("mcp %s: stdout pipe: %w", name, err)
	}

	// Drain stderr to a per-server log file.
	//
	// Draining is mandatory: an unread pipe fills its buffer (64KB on macOS)
	// and then blocks the server's next write, stranding it mid-response and
	// looking exactly like a hung tool.
	//
	// Where it goes matters too. The MCP docs are explicit that "stdio
	// servers may use stderr for all their logging", so this is not an error
	// channel — it is the server's entire diagnostic output, and discarding
	// it means a server that fails to start gives the user nothing to look
	// at. Claude Desktop writes mcp-server-NAME.log per server; same idea
	// here, under ~/.localkin/logs/.
	//
	// Truncated per launch rather than appended: the useful question is
	// always "why did it fail just now", and an ever-growing file for a
	// long-lived desktop process is a disk leak nobody remembers to rotate.
	logFile := serverLogFile(name)
	if errPipe, err := cmd.StderrPipe(); err == nil {
		var sink io.Writer = io.Discard
		if logFile != nil {
			sink = logFile
		}
		go func() { _, _ = io.Copy(sink, errPipe) }()
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("mcp %s: start: %w", name, err)
	}

	c := &Client{
		name:      name,
		cmd:       cmd,
		stdin:     stdin,
		stdout:    bufio.NewScanner(stdout),
		stderrLog: logFile,
	}
	c.stdout.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer

	if err := c.initialize(); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialize: %w", name, err)
	}

	return c, nil
}

// Name returns the server name.
func (c *Client) Name() string { return c.name }

// Tools returns the cached tool definitions.
func (c *Client) Tools() []ToolDef { return c.tools }

func (c *Client) initialize() error {
	_, err := c.call("initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    struct{}{},
		ClientInfo: clientInfo{
			Name:    "kinclaw",
			Version: "0.1.0",
		},
	})
	if err != nil {
		return fmt.Errorf("initialize request: %w", err)
	}

	if err := c.notify("notifications/initialized", nil); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	return nil
}

// ListTools sends tools/list and caches the result.
func (c *Client) ListTools() ([]ToolDef, error) {
	raw, err := c.call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	var result toolsListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse tools/list result: %w", err)
	}
	c.tools = result.Tools
	return result.Tools, nil
}

// CallTool invokes a tool on the server.
func (c *Client) CallTool(toolName string, args map[string]any) (string, error) {
	raw, err := c.call("tools/call", callToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return "", err
	}

	var result callToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parse tools/call result: %w", err)
	}

	var text string
	for _, block := range result.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}

	if result.IsError {
		return "", fmt.Errorf("mcp tool error: %s", text)
	}
	return text, nil
}

// Close shuts down the MCP server process.
func (c *Client) Close() {
	c.closed.Store(true)
	if c.stdin != nil {
		c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
	if c.stderrLog != nil {
		_ = c.stderrLog.Close()
		c.stderrLog = nil
	}
}

// call sends a JSON-RPC request and waits for the matching response.
func (c *Client) call(method string, params any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("mcp %s: client closed", c.name)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID.Add(1)
	req := jsonrpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	if err := c.send(req); err != nil {
		return nil, err
	}

	// The scanner has no deadline of its own, so run the read on a goroutine
	// and race it against a timer. On timeout the client is closed: the
	// stream position is no longer known (a late reply would be read as the
	// answer to whatever is asked next), and a half-synchronised stdio
	// channel is not worth trying to resynchronise.
	type readResult struct {
		raw json.RawMessage
		err error
	}
	done := make(chan readResult, 1)
	go func() {
		raw, err := c.readResponse(id)
		done <- readResult{raw, err}
	}()

	select {
	case r := <-done:
		return r.raw, r.err
	case <-time.After(callTimeout):
		go c.Close()
		return nil, fmt.Errorf("mcp %s: %s timed out after %s", c.name, method, callTimeout)
	}
}

// readResponse scans until the response with the given id arrives.
func (c *Client) readResponse(id int64) (json.RawMessage, error) {
	for {
		if !c.stdout.Scan() {
			if err := c.stdout.Err(); err != nil {
				return nil, fmt.Errorf("read response: %w", err)
			}
			return nil, fmt.Errorf("server closed connection")
		}

		line := c.stdout.Bytes()
		if len(line) == 0 {
			continue
		}

		var resp jsonrpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Not JSON-RPC — a stray log line on stdout. Skip.
			continue
		}
		if resp.ID == nil || *resp.ID != id {
			// Notification, or an answer to a request we gave up on.
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (c *Client) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.send(jsonrpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	data = append(data, '\n')
	_, err = c.stdin.Write(data)
	return err
}

// LogDir is where per-server stderr logs are written.
func LogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".localkin", "logs")
}

// ServerLogPath is the log file for one server, so callers (and a future
// settings UI) can point the user at it by name.
func ServerLogPath(name string) string {
	dir := LogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "mcp-server-"+name+".log")
}

// serverLogFile opens a server's log for writing, truncating any previous
// run. Returns nil on failure — losing diagnostics is not a reason to refuse
// to start the server.
func serverLogFile(name string) *os.File {
	path := ServerLogPath(name)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil
	}
	return f
}
