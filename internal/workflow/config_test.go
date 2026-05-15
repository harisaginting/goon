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
	// Chdir into an empty tmp dir so the new ./workflow.json default
	// can't accidentally pick up a real file from the test's CWD.
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
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

// TestLoadConfig_RepoRootWins covers the new resolution priority: a
// workflow.json sitting in the repo root takes precedence over legacy
// .goon/workflow.json paths. This is the path `goon workflow init`
// writes to, so it must be the path goon reads from first.
func TestLoadConfig_RepoRootWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GOON_WORKFLOW_FILE", "")
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	// Both repo-root and legacy .goon/ contain configs; repo-root must win.
	_ = os.WriteFile(filepath.Join(dir, "workflow.json"),
		[]byte(`{"branch_prefix":"root/"}`), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, ".goon"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".goon", "workflow.json"),
		[]byte(`{"branch_prefix":"legacy/"}`), 0o644)

	cfg, src, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BranchPrefix != "root/" {
		t.Errorf("repo root should win; got branch_prefix=%q", cfg.BranchPrefix)
	}
	if !strings.HasSuffix(src, "/workflow.json") || strings.Contains(src, ".goon") {
		t.Errorf("source should be repo-root workflow.json, got %q", src)
	}
}

// TestDefaultConfigFilePath_RepoRoot guards against silently regressing
// the new default — `goon workflow init` must write to ./workflow.json,
// not back into ~/.config/goon/.
func TestDefaultConfigFilePath_RepoRoot(t *testing.T) {
	t.Setenv("GOON_WORKFLOW_FILE", "")
	t.Setenv("XDG_CONFIG_HOME", "/should-not-be-consulted")
	t.Setenv("HOME", "/also-should-not-be-consulted")
	got := DefaultConfigFilePath()
	if !strings.HasSuffix(got, "workflow.json") {
		t.Errorf("DefaultConfigFilePath should end in workflow.json; got %q", got)
	}
	if strings.Contains(got, ".goon") || strings.Contains(got, ".config") {
		t.Errorf("DefaultConfigFilePath leaked legacy path; got %q", got)
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
	// Chdir to tmp so the new ./workflow.json default doesn't pick up
	// a stray file from whatever dir `go test` was launched in.
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

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

// TestSaveDefault_WritesEducationalStarter covers the v0.2 contract:
// `goon workflow init` writes a comprehensive starter with every hook
// represented and a few populated with self-documenting echo commands so
// a first-time user can read the JSON top-to-bottom without consulting
// docs. Regression risk is high if a future refactor reverts this to
// empty-array hooks (loses onboarding clarity silently).
func TestSaveDefault_WritesEducationalStarter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")
	if err := SaveDefault(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, _ := os.ReadFile(path)
	var cfg WorkflowConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if cfg.AutoApprove {
		t.Error("starter should default auto_approve=false (gates fire)")
	}
	if cfg.VerifyRuns != 3 {
		t.Errorf("starter verify_runs = %d, want 3", cfg.VerifyRuns)
	}
	if len(cfg.ExtraLabels) == 0 {
		t.Error("starter should have extra_labels populated for visibility")
	}
	// Hooks must include educational echo commands at the obvious entry
	// points, not be entirely empty.
	for _, k := range []string{HookBeforeTriage, HookAfterExecute, HookBeforePR, HookAfterPR, HookOnFailure} {
		if len(cfg.Hooks[k]) == 0 {
			t.Errorf("starter %s hook should have an example command, got empty", k)
		}
	}
	// Description should mention the gates so the user knows what
	// auto_approve toggles.
	for _, want := range []string{"confirm_repo", "approve_plan", "auto_approve"} {
		if !strings.Contains(cfg.Description, want) {
			t.Errorf("starter description missing %q: %q", want, cfg.Description)
		}
	}
}

func TestBranchName(t *testing.T) {
	cases := []struct{ prefix, key, want string }{
		// Preserves case — the canonical Jira-style key passes
		// through verbatim. Lowercasing was removed because users
		// expect "EB-4795" on the branch list, not "eb-4795".
		{"", "ENG-1", "goon/ENG-1"},
		{"goon/", "ENG-1", "goon/ENG-1"},
		{"feature/", "ENG-1", "feature/ENG-1"},
		{"goon-", "ENG-1", "goon-ENG-1"},
		{"bot_", "X-9", "bot_X-9"},
		// Real-world Jira project + ticket id stays readable.
		{"", "EB-4795", "goon/EB-4795"},
		// Disallowed characters collapse to dashes; consecutive
		// dashes squash; edge dashes get trimmed.
		{"", "EB 4795", "goon/EB-4795"},
		{"", "GH-#42", "goon/GH-42"},
		{"", "  ENG-1  ", "goon/ENG-1"},
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

// TestDefaultConfig_HasName guards the contract that DefaultConfig() always
// produces a non-empty Name — startup code uses this name in stdout/log lines
// to identify the active workflow.
func TestDefaultConfig_HasName(t *testing.T) {
	c := DefaultConfig()
	if c.Name == "" {
		t.Error("DefaultConfig().Name is empty; expected a non-empty default")
	}
	if c.Description == "" {
		t.Error("DefaultConfig().Description is empty; expected a sensible default")
	}
}

// TestLoadConfig_Name verifies a custom name in the JSON file is preserved
// through merge() and surfaces on the loaded config — this is the value
// printed at startup so the user knows which workflow is active.
func TestLoadConfig_Name(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")
	contents := `{
	  "name": "my-eng-pipeline",
	  "description": "custom pipeline for repo X",
	  "branch_prefix": "goon/"
	}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOON_WORKFLOW_FILE", path)
	cfg, _, err := LoadConfig("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Name != "my-eng-pipeline" {
		t.Errorf("Name: got %q want %q", cfg.Name, "my-eng-pipeline")
	}
	if cfg.Description != "custom pipeline for repo X" {
		t.Errorf("Description: got %q", cfg.Description)
	}
}

// TestMerge_NameOverridesDefault checks that a partial config's Name
// replaces the default — without this, every workflow would silently
// announce itself as "default" no matter what the JSON says.
func TestMerge_NameOverridesDefault(t *testing.T) {
	base := DefaultConfig()
	if base.Name != "default" {
		t.Fatalf("precondition: default name should be %q, got %q", "default", base.Name)
	}
	partial := WorkflowConfig{Name: "marketing-brief", Description: "campaign pipeline"}
	merge(&base, partial)
	if base.Name != "marketing-brief" {
		t.Errorf("Name not overridden: %q", base.Name)
	}
	if base.Description != "campaign pipeline" {
		t.Errorf("Description not overridden: %q", base.Description)
	}
}

// TestAnnounce_PrintsName covers the user-visible startup banner. We call
// Announce with no config file present and assert the default name appears
// in the printed line — that's what `goon start` shows on its first line.
func TestAnnounce_PrintsName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GOON_WORKFLOW_FILE", "")

	var buf strings.Builder
	cfg, src := Announce("", &buf)
	if cfg.Name == "" {
		t.Error("Announce returned empty Name")
	}
	if src != "" {
		t.Errorf("expected no source for default config, got %q", src)
	}
	out := buf.String()
	if !strings.Contains(out, "workflow:") {
		t.Errorf("banner missing 'workflow:' prefix: %q", out)
	}
	if !strings.Contains(out, cfg.Name) {
		t.Errorf("banner missing name %q: %q", cfg.Name, out)
	}
}

// TestAnnounce_PrintsCustomName: when a workflow JSON declares a name, that
// is what shows up in the banner — not "default" — so an operator running
// `goon start` against, say, the marketing pipeline sees that name.
func TestAnnounce_PrintsCustomName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")
	if err := os.WriteFile(path, []byte(`{
	  "name": "marketing-brief",
	  "description": "campaign pipeline",
	  "stages": [{"name":"x","type":"llm","prompt":"hi"}]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOON_WORKFLOW_FILE", path)

	var buf strings.Builder
	cfg, src := Announce("", &buf)
	if cfg.Name != "marketing-brief" {
		t.Errorf("Name: got %q want %q", cfg.Name, "marketing-brief")
	}
	if src != path {
		t.Errorf("source: got %q want %q", src, path)
	}
	out := buf.String()
	if !strings.Contains(out, "marketing-brief") {
		t.Errorf("banner missing custom name: %q", out)
	}
	if !strings.Contains(out, "1 stage") {
		t.Errorf("banner missing stage count: %q", out)
	}
	if !strings.Contains(out, "campaign pipeline") {
		t.Errorf("banner missing description: %q", out)
	}
}
