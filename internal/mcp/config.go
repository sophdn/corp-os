package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// The mounted-server type tags a Config entry carries. corpos owns the loop, so
// it mounts a heterogeneous set of MCP servers and projects a lean per-profile
// subset of their union (design doc §1.2/§5#4). Each server declares its type so
// the composition root knows which provider to construct.
const (
	// ServerTypeToolkitHTTP is the bespoke POST /mcp/<surface> toolkit-server
	// dispatch (NOT MCP-spec JSON-RPC — see the T3 client-choice deliverable).
	ServerTypeToolkitHTTP = "toolkit-http"
	// ServerTypeWeb is the owned web surface (web.search / web.fetch).
	ServerTypeWeb = "web"
)

// DefaultToolkitURL is the single canonical toolkit-server endpoint for host
// clients. Post-flip (chain auto-startup-dev-services T7) the canonical surface is
// the containerized toolkit on :3001; the native :3000 daemon is RETIRED. This is
// the one place corpos's default endpoint is defined — change it here, not in
// scattered literals (see mcp-servers/docs/TOPOLOGY.md §"linking law"). In-container
// callers use the corpos-net DNS name instead.
const DefaultToolkitURL = "http://localhost:3001"

// knownServerTypes is the closed set Validate accepts; an unknown type is a
// config error (fail-closed: corpos won't silently skip a server it can't build).
var knownServerTypes = map[string]bool{
	ServerTypeToolkitHTTP: true,
	ServerTypeWeb:         true,
}

// ServerConfig is one mounted MCP server in the .mcp.json-equivalent config. The
// field set is the union across server types; which fields are load-bearing
// depends on Type (URL for toolkit-http, SearchURL/UserAgent for web). Unused
// fields for a given type are simply ignored.
type ServerConfig struct {
	// Type selects the provider to build (see ServerType* constants).
	Type string `json:"type"`
	// URL is the toolkit-http daemon base URL (e.g. DefaultToolkitURL, :3001).
	URL string `json:"url,omitempty"`
	// Project is the default project scope sent on toolkit-http write calls.
	Project string `json:"project,omitempty"`
	// SearchURL overrides the web server's search endpoint (default DDG html).
	SearchURL string `json:"search_url,omitempty"`
	// UserAgent overrides the web server's outbound User-Agent header.
	UserAgent string `json:"user_agent,omitempty"`
}

// Config is the parsed .mcp.json-equivalent: a name→server map under the
// "mcpServers" key, mirroring Claude Code's .mcp.json shape so the format is
// familiar. The map key is the server's mount name (diagnostics only — surfaces,
// not server names, are what route).
type Config struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}

// DefaultConfig is the built-in two-server mount used when no -mcp-config file is
// given: the toolkit-server (all its surfaces) plus the owned web surface. Web is
// mounted unconditionally because per-profile projection (task 3) is what keeps it
// out of the workers that don't declare it — owning the surface is cheap, the
// projection makes it lean. mcpURL/project come from the CLI flags.
func DefaultConfig(mcpURL, project string) Config {
	return Config{Servers: map[string]ServerConfig{
		"toolkit": {Type: ServerTypeToolkitHTTP, URL: mcpURL, Project: project},
		"web":     {Type: ServerTypeWeb},
	}}
}

// ParseConfig decodes and validates a .mcp.json-equivalent config from raw JSON.
// Decoding is strict (unknown top-level/server fields are rejected) so a typo'd
// key surfaces as an error instead of being silently dropped.
func ParseConfig(data []byte) (Config, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse mcp config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// LoadConfig reads and parses a .mcp.json-equivalent config file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read mcp config %s: %w", path, err)
	}
	return ParseConfig(data)
}

// Validate checks the config mounts at least one server and that every server has
// a known type plus the fields that type requires. It returns the first problem.
func (c Config) Validate() error {
	if len(c.Servers) == 0 {
		return fmt.Errorf("mcp config mounts no servers (need at least one under \"mcpServers\")")
	}
	for _, name := range c.Names() {
		s := c.Servers[name]
		if !knownServerTypes[s.Type] {
			return fmt.Errorf("server %q has unknown type %q (want %s|%s)", name, s.Type, ServerTypeToolkitHTTP, ServerTypeWeb)
		}
		if s.Type == ServerTypeToolkitHTTP && strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("server %q (%s) has no url", name, ServerTypeToolkitHTTP)
		}
	}
	return nil
}

// Names returns the configured server mount names, sorted — a deterministic order
// for diagnostics and for building servers in a stable sequence.
func (c Config) Names() []string {
	out := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
