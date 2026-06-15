package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExampleWorkflows_AllValid parses every examples/workflows/*.json preset
// and runs the same validation the daemon enforces, so the copy-paste library
// can never ship an unrunnable or stale-schema config (e.g. the removed
// llm/agent/http stage types).
func TestExampleWorkflows_AllValid(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		count++
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var cfg WorkflowConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			// Stage-based presets must use only the new role types — guard
			// against a stale llm/agent/http slipping back in.
			for _, s := range cfg.Stages {
				switch strings.ToLower(strings.TrimSpace(s.Type)) {
				case RoleAnalyst, RoleExecutor, RoleReviewer, RoleLoop, RoleNotify:
				default:
					t.Errorf("stage %q has non-role type %q", s.Name, s.Type)
				}
			}
		})
	}
	if count == 0 {
		t.Fatal("no example workflows found")
	}
}
