package config

import "testing"

func TestApprovalEnvMapping(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x")
	t.Setenv("DBB_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	t.Setenv("DBB_APPROVAL_ENABLED", "true")
	t.Setenv("DBB_APPROVAL_SLACK_DELAY", "5s")
	t.Setenv("DBB_APPROVAL_SLACK_SQL", "false")

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Approval.Enabled {
		t.Fatal("enabled not picked up")
	}
	if cfg.Approval.SlackDelayDuration().String() != "5s" {
		t.Fatalf("delay %v", cfg.Approval.SlackDelayDuration())
	}
	if cfg.Approval.SlackSQL {
		t.Fatal("slack_sql not picked up")
	}
}

func TestApprovalDefaults(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x")
	t.Setenv("DBB_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Approval.Enabled {
		t.Fatal("approval must default off")
	}
	if !cfg.Approval.SlackSQL {
		t.Fatal("slack sql must default on")
	}
	if cfg.Approval.SlackDelayDuration().String() != "30s" {
		t.Fatalf("delay %v", cfg.Approval.SlackDelayDuration())
	}
}
