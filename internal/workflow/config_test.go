package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig_HasRequiredFields(t *testing.T) {
	c := DefaultConfig()
	if c.BranchPrefix != "goon/" {
		t.Errorf("branch prefix: %q", c.BranchPrefix)
	}
	if c.PRTitleTemplate == "" || c.PRBodyTemplate == "" {
		t.Errorf("templates should be set")
	}
	for _, h := range AllHooks {
		if _, ok := c.Hooks[h]; !ok {
			t.Errorf("default config missing hook key: %s", h)
		}
	}
}

func TestLoadConfig_NoFile_ReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GOON_WORKFLOW_FILE", "")
	cfg, src, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if src != "" {
		t.Errorf("expected no source, got %q", src)
	}
	if cfg.BranchPrefix != "goon/" {
		t.Errorf("expected default branch prefix, got %q", cfg.BranchPrefix)
	}
}

func TestLoadConfig_ExplicitFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")
	contents := `{
	  "branch_prefix": "feature/",
	  "test_command": "make verify",
	  "verify_runs": 5,
	  "hooks": {"before_pr": ["go fmt ./...", "git diff --stat"]}
	}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOON_WORKFLOW_FILE", path)
	cfg, src, err := LoadConfig("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if src != path {
		t.Errorf("source: %q want %q", src, path)
	}
	if cfg.BranchPrefix != "feature/" {
		t.Errorf("branch_prefix: %q", cfg.BranchPrefix)
	}
	if cfg.TestCommand != "make verify" {
		t.Errorf("test_command: %q", cfg.TestCommand)
	}
	if cfg.VerifyRuns != 5 {
		t.Errorf("verify_runs: %d", cfg.VerifyRuns)
	}
	got := cfg.Hook(HookBeforePR)
	if len(got) != 2 || got[0] != "go fmt ./..." {
		t.Errorf("before_pr: %+v", got)
	}
	// PR templates should still be defaults (not overridden in file).
	if !strings.Contains(cfg.PRTitleTemplate, "{{.Key}}") {
		t.Errorf("PRTitleTemplate lost: %q", cfg.PRTitleTemplate)
	}
}

func TestLoadConfig_RepoLocalWins(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "myrepo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".goon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".goon", "workflow.json"),
		[]byte(`{"branch_prefix":"repolocal/"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GOON_WORKFLOW_FILE", "")

	cfg, src, err := LoadConfig(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BranchPrefix != "repolocal/" {
		t.Errorf("got: %q", cfg.BranchPrefix)
	}
	if !strings.Contains(src, "myrepo/.goon/workflow.json") {
		t.Errorf("source: %s", src)
	}
}

func TestLoadConfig_BadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o644)
	t.Setenv("GOON_WORKFLOW_FILE", path)
	_, _, err := LoadConfig("")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSaveDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "goon", "workflow.json")
	if err := SaveDefault(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not written: %v", err)
	}
	data, _ := os.ReadFile(path)
	var got WorkflowConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("invalid JSON written: %v", err)
	}
	if got.BranchPrefix != "goon/" {
		t.Errorf("default prefix lost: %q", got.BranchPrefix)
	}
	for _, h := range AllHooks {
		if _, ok := got.Hooks[h]; !ok {
			t.Errorf("starter file missing %s key", h)
		}
	}
}

func TestSaveDefault_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")
	_ = os.WriteFile(path, []byte("existing"), 0o644)
	if err := SaveDefault(path); err == nil {
		t.Fatal("expected refusal to overwrite")
	}
}

func TestBranchName(t *testing.T) {
	cases := []struct{ prefix, key, want string }{
		{"", "ENG-1", "goon/eng-1"},
		{"goon/", "ENG-1", "goon/eng-1"},
		{"feature/", "ENG-1", "feature/eng-1"},
		{"goon-", "ENG-1", "goon-eng-1"},
		{"bot_", "X-9", "bot_x-9"},
	}
	for _, c := range cases {
		got := branchName(c.prefix, c.key)
		if got != c.want {
			t.Errorf("branchName(%q,%q) = %q want %q", c.prefix, c.key, got, c.want)
		}
	}
}

func TestMerge_OverlaysOnlyNonZero(t *testing.T) {
	base := DefaultConfig()
	partial := WorkflowConfig{TestCommand: "go test -race ./..."}
	merge(&base, partial)
	if base.TestCommand != "go test -race ./..." {
		t.Errorf("test_command not applied: %q", base.TestCommand)
	}
	if base.BranchPrefix != "goon/" {
		t.Errorf("branch_prefix should remain default: %q", base.BranchPrefix)
	}
}
