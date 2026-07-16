package mcp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultConfig_MountsToolkitAndWeb(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig("http://localhost:3000", "mcp-servers")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if got := cfg.Names(); !reflect.DeepEqual(got, []string{"toolkit", "web"}) {
		t.Fatalf("names = %v, want [toolkit web]", got)
	}
	tk := cfg.Servers["toolkit"]
	if tk.Type != ServerTypeToolkitHTTP || tk.URL != "http://localhost:3000" || tk.Project != "mcp-servers" {
		t.Fatalf("toolkit entry = %+v", tk)
	}
	if cfg.Servers["web"].Type != ServerTypeWeb {
		t.Fatalf("web type = %q", cfg.Servers["web"].Type)
	}
}

func TestParseConfig_Valid(t *testing.T) {
	t.Parallel()
	raw := `{"mcpServers":{
		"toolkit":{"type":"toolkit-http","url":"http://x:3000","project":"p"},
		"web":{"type":"web","search_url":"https://example/s","user_agent":"corpos-test"}
	}}`
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(cfg.Servers))
	}
	w := cfg.Servers["web"]
	if w.SearchURL != "https://example/s" || w.UserAgent != "corpos-test" {
		t.Fatalf("web entry = %+v", w)
	}
}

func TestParseConfig_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := ParseConfig([]byte(`{"mcpServers":{"web":{"type":"web","bogus":1}}}`))
	if err == nil || !strings.Contains(err.Error(), "parse mcp config") {
		t.Fatalf("err = %v, want parse error on unknown field", err)
	}
}

func TestParseConfig_RejectsEmptyAndBadTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, raw, want string
	}{
		{"no servers", `{"mcpServers":{}}`, "mounts no servers"},
		{"missing key", `{}`, "mounts no servers"},
		{"unknown type", `{"mcpServers":{"x":{"type":"stdio"}}}`, "unknown type"},
		{"toolkit no url", `{"mcpServers":{"t":{"type":"toolkit-http"}}}`, "has no url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

func TestParseConfig_WebNeedsNoURL(t *testing.T) {
	t.Parallel()
	if _, err := ParseConfig([]byte(`{"mcpServers":{"web":{"type":"web"}}}`)); err != nil {
		t.Fatalf("web-only config should validate: %v", err)
	}
}

func TestParseConfig_MalformedJSON(t *testing.T) {
	t.Parallel()
	if _, err := ParseConfig([]byte(`{not json`)); err == nil {
		t.Fatal("want error on malformed JSON")
	}
}

func TestLoadConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	raw := `{"mcpServers":{"toolkit":{"type":"toolkit-http","url":"http://h:3000"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Servers["toolkit"].URL != "http://h:3000" {
		t.Fatalf("loaded url = %q", cfg.Servers["toolkit"].URL)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil || !strings.Contains(err.Error(), "read mcp config") {
		t.Fatalf("err = %v, want read error", err)
	}
}
