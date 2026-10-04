package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadKeepsDefaults(t *testing.T) {
	token := writeFile(t, "token", "perm:abc\n")
	path := writeFile(t, "config.yml", `
youtrack:
  url: https://youtrack.example.com
  token_file: `+token+`
collectors:
  projects:
    include_archived: true
  issue_types:
    include: [ABC]
  users:
  queries:
    items:
      - name: critical
        query: "#Unresolved Priority: Critical"
        labels: {team: ops}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.YouTrack.Token != "perm:abc" {
		t.Errorf("token = %q", cfg.YouTrack.Token)
	}
	if cfg.YouTrack.Timeout != 10*time.Second || cfg.YouTrack.MaxConcurrentRequests != 4 {
		t.Errorf("youtrack defaults lost: %+v", cfg.YouTrack)
	}
	p := cfg.Collectors.Projects
	if !p.Enabled || !p.IncludeArchived || p.Interval != 5*time.Minute {
		t.Errorf("projects = %+v", p)
	}
	it := cfg.Collectors.IssueTypes
	if it.Enabled || it.Interval != 2*time.Minute || it.FullResyncInterval != 24*time.Hour || it.TypeField != "Type" ||
		len(it.Include) != 1 {
		t.Errorf("issue_types = %+v", it)
	}
	if cfg.Collectors.Users.Interval != 15*time.Minute {
		t.Errorf("users interval = %s", cfg.Collectors.Users.Interval)
	}
	if !cfg.Collectors.Telemetry.Enabled || len(cfg.Collectors.Queries.Items) != 1 {
		t.Errorf("collectors = %+v", cfg.Collectors)
	}
}

func TestLoadEnvironment(t *testing.T) {
	t.Setenv("YOUTRACK_URL", "http://127.0.0.1:8080")
	t.Setenv("YOUTRACK_TOKEN", "perm:env")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.YouTrack.URL != "http://127.0.0.1:8080" || cfg.YouTrack.Token != "perm:env" {
		t.Errorf("youtrack = %+v", cfg.YouTrack)
	}
}

func TestLoadInvalid(t *testing.T) {
	path := writeFile(t, "config.yml", `
youtrack:
  url: youtrack.example.com
collectors:
  issue_types:
    enabled: true
    interval: 1s
    full_resync_interval: 500ms
  users:
    interval: 1s
  queries:
    items:
      - {name: a, query: x, labels: {name: b}}
      - {name: a, query: y}
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"youtrack.url", "token is required", "issue_types.interval", "full_resync_interval", "users.interval", "invalid label name", "duplicate name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadUnknownField(t *testing.T) {
	path := writeFile(t, "config.yml", "youtrack:\n  ulr: http://x\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "ulr") {
		t.Fatalf("err = %v, want unknown field error", err)
	}
}
