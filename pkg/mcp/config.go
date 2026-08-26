package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/LocalKinAI/kinclaw/pkg/skill"
)

// ServerConfig describes one MCP server to launch.
type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Disabled keeps a server in the file but stops it from launching, so
	// turning one off in the UI doesn't mean destroying its configuration
	// (command, args, and API keys) and retyping it later.
	Disabled bool `json:"disabled,omitempty"`
}

// Config is the on-disk format.
//
// The field is `mcpServers` because that is what Claude Desktop, kincode and
// the wider ecosystem use. Matching it means a server's published install
// snippet can be pasted in unchanged, and an existing config can be copied
// over wholesale — a private format would buy nothing and cost that.
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// DefaultConfigPath is ~/.localkin/mcp.json.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".localkin", "mcp.json")
}

// LoadConfig reads the config file. A missing file is not an error — it is
// the normal state for anyone not using MCP, and must not be reported as a
// failure on every launch.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{MCPServers: map[string]ServerConfig{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ServerConfig{}
	}
	return &cfg, nil
}

// LoadResult reports what happened for one configured server, so the caller
// can log it and a settings UI can show why a server isn't providing tools.
type LoadResult struct {
	Name      string
	Disabled  bool
	ToolCount int
	Err       error
}

// Connect launches every enabled server and registers its tools.
//
// One server failing must not stop the others: these are third-party
// subprocesses whose binaries may be missing, whose API keys may have
// expired, and whose startup may fail for reasons that have nothing to do
// with the rest of the agent. A broken entry costs its own tools and nothing
// more.
//
// Returns the live clients so the caller can close them at shutdown.
func Connect(cfg *Config, reg *skill.Registry) ([]*Client, []LoadResult) {
	var clients []*Client
	var results []LoadResult

	// Deterministic order, so logs and tool listings don't reshuffle between
	// launches purely because Go randomises map iteration.
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		srv := cfg.MCPServers[name]

		if srv.Disabled {
			results = append(results, LoadResult{Name: name, Disabled: true})
			continue
		}
		if srv.Command == "" {
			results = append(results, LoadResult{
				Name: name,
				Err:  fmt.Errorf("no command specified"),
			})
			continue
		}

		env := make([]string, 0, len(srv.Env))
		for k, v := range srv.Env {
			env = append(env, k+"="+v)
		}
		sort.Strings(env)

		client, err := ConnectServer(name, srv.Command, srv.Args, env)
		if err != nil {
			// Point at the log rather than making the user find it. A stdio
			// server that dies on startup usually explains itself on stderr,
			// and that text is the difference between "it didn't work" and
			// "missing API key" / "package not found".
			if p := ServerLogPath(name); p != "" {
				err = fmt.Errorf("%w (server output: %s)", err, p)
			}
			results = append(results, LoadResult{Name: name, Err: err})
			continue
		}

		tools, err := client.ListTools()
		if err != nil {
			client.Close()
			results = append(results, LoadResult{
				Name: name,
				Err:  fmt.Errorf("list tools: %w", err),
			})
			continue
		}

		for _, s := range SkillsFromClient(client) {
			reg.Register(s)
		}

		clients = append(clients, client)
		results = append(results, LoadResult{Name: name, ToolCount: len(tools)})
	}

	return clients, results
}

// ConnectServer is Connect for a single server, kept separate so the
// package-level Connect name can take the config-driven form.
func ConnectServer(name, command string, args []string, env []string) (*Client, error) {
	return connect(name, command, args, env)
}
