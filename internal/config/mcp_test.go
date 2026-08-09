package config

import "testing"

// TestDefaultConfigEnablesMCP pins the shipped default. The API package
// registers the /mcp routes off this flag, so a regression here would silently
// remove the endpoint from every deployment that never sets the variable.
func TestDefaultConfigEnablesMCP(t *testing.T) {
	if !defaultConfig().MCP.Enabled {
		t.Fatal("MCP must default on")
	}

	t.Setenv("DBB_DSN", "postgres://x")
	t.Setenv("DBB_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if !cfg.MCP.Enabled {
		t.Fatal("MCP must default on after a full load")
	}
}

func TestMCPEnvMapping(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x")
	t.Setenv("DBB_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	t.Setenv("DBB_MCP_ENABLED", "false")

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.MCP.Enabled {
		t.Fatal("DBB_MCP_ENABLED=false was not picked up")
	}
}
