package workflow

import "testing"

// TestStarterTemplates_AllValid ensures every embedded scaffold parses and
// passes the same validation the daemon enforces — so the editor's
// "start from template" dropdown can never seed an unrunnable config.
func TestStarterTemplates_AllValid(t *testing.T) {
	ts, err := StarterTemplates()
	if err != nil {
		t.Fatalf("StarterTemplates: %v", err)
	}
	if len(ts) == 0 {
		t.Fatal("no starter templates returned")
	}
	seenKeys := map[string]bool{}
	for _, st := range ts {
		if st.Key == "" || st.Label == "" {
			t.Errorf("template missing key/label: %+v", st)
		}
		if seenKeys[st.Key] {
			t.Errorf("duplicate template key %q", st.Key)
		}
		seenKeys[st.Key] = true
		if err := st.Config.Validate(); err != nil {
			t.Errorf("template %q failed validation: %v", st.Key, err)
		}
	}
	// The first template is the canonical built-in seed.
	if ts[0].Key != "default" {
		t.Errorf("first template key = %q, want \"default\"", ts[0].Key)
	}
}

// TestBuiltinStageSeed_NonEmpty guards the fix for "edit pipeline shows a
// blank canvas": the editor seeds from this when the loaded config has no
// stages, so it must return runnable stages.
func TestBuiltinStageSeed_NonEmpty(t *testing.T) {
	seed := BuiltinStageSeed()
	if len(seed) == 0 {
		t.Fatal("BuiltinStageSeed returned no stages")
	}
	if err := validateStages(seed); err != nil {
		t.Fatalf("builtin seed is not runnable: %v", err)
	}
	// Mutating the returned slice must not corrupt the cached parse.
	seed[0].Name = "mutated"
	again := BuiltinStageSeed()
	if again[0].Name == "mutated" {
		t.Error("BuiltinStageSeed leaked its backing array — callers can mutate the seed")
	}
}

// TestValidate catches the schema problems the web save handler relies on it
// to reject before writing workflow.json.
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     WorkflowConfig
		wantErr bool
	}{
		{
			name:    "no stages is fine (built-in pipeline)",
			cfg:     WorkflowConfig{Name: "x"},
			wantErr: false,
		},
		{
			name: "valid llm + agent",
			cfg: WorkflowConfig{Stages: []StageConfig{
				{Name: "a", Type: "llm", Prompt: "hi"},
				{Name: "b", Type: "agent", Task: "do"},
			}},
			wantErr: false,
		},
		{
			name: "duplicate stage names",
			cfg: WorkflowConfig{Stages: []StageConfig{
				{Name: "a", Type: "llm", Prompt: "hi"},
				{Name: "a", Type: "agent", Task: "do"},
			}},
			wantErr: true,
		},
		{
			name: "llm missing prompt",
			cfg: WorkflowConfig{Stages: []StageConfig{
				{Name: "a", Type: "llm"},
			}},
			wantErr: true,
		},
		{
			name: "agent missing task",
			cfg: WorkflowConfig{Stages: []StageConfig{
				{Name: "a", Type: "agent"},
			}},
			wantErr: true,
		},
		{
			name: "unknown type",
			cfg: WorkflowConfig{Stages: []StageConfig{
				{Name: "a", Type: "wat", Prompt: "x"},
			}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
