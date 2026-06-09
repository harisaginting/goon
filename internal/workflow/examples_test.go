package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the test's CWD until it finds the workspace root
// (the directory containing go.mod). Lets the integration tests work
// regardless of where `go test` is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := cwd; dir != "/" && dir != ""; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	t.Fatalf("could not find repo root (go.mod) starting from %s", cwd)
	return ""
}

// loadExample reads + JSON-decodes one of the shipped preset files. Fails the
// test loudly if the JSON is malformed — these are user-facing examples and
// any breakage in them is a release-blocker.
func loadExample(t *testing.T, name string) WorkflowConfig {
	t.Helper()
	path := filepath.Join(repoRoot(t), "examples", "workflows", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg WorkflowConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(cfg.Stages) == 0 {
		t.Fatalf("%s: no stages defined", path)
	}
	if err := validateStages(cfg.Stages); err != nil {
		t.Fatalf("%s: validateStages: %v", path, err)
	}
	return cfg
}

// TestExample_Engineering runs the engineering-stages preset end-to-end
// against a mock LLM. Walks each stage, asserting that prior-stage output
// flows into the next stage's prompt/task as expected.
func TestExample_Engineering(t *testing.T) {
	cfg := loadExample(t, "engineering-stages.json")

	// Replies, in order:
	//  1. triage llm    → JSON plan with two steps
	//  2. execute agent → finish
	//  3. verify agent  → finish (pass 1/3)
	//  4. verify agent  → finish (pass 2/3)
	//  5. verify agent  → finish (pass 3/3)
	replies := []string{
		`{"steps":[{"title":"add OAuth handler"},{"title":"wire route"}]}`,
		`{"tool":"finish","args":{"message":"step done"}}`,
		`{"tool":"finish","args":{"message":"verified"}}`,
		`{"tool":"finish","args":{"message":"verified"}}`,
		`{"tool":"finish","args":{"message":"verified"}}`,
	}
	r, mock, out := newRunner(t, replies)

	state, err := r.RunStages(context.Background(), cfg, ticketFixture(), "goon/eng-1", "")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if mock.Calls != len(replies) {
		t.Fatalf("LLM calls = %d; want %d\n%s", mock.Calls, len(replies), out.String())
	}

	// triage produced the parsed JSON
	triage, ok := state.Stages["triage"].(map[string]any)
	if !ok {
		t.Fatalf("triage output type = %T", state.Stages["triage"])
	}
	steps, _ := triage["steps"].([]any)
	if len(steps) != 2 {
		t.Errorf("steps len = %d; want 2", len(steps))
	}

	// execute stage stored the rendered task string
	if _, ok := state.Stages["execute"].(string); !ok {
		t.Errorf("execute output type = %T (want string)", state.Stages["execute"])
	}

	// Trace all recorded prompts/tasks. The execute task must contain the
	// triage steps interpolated via the {{range}} block.
	var sawExecutePrompt bool
	for _, msgs := range mock.AllMsgs {
		for _, msg := range msgs {
			c := msg.Content
			if strings.Contains(c, "add OAuth handler") && strings.Contains(c, "wire route") &&
				strings.Contains(c, "Implement ticket") {
				sawExecutePrompt = true
			}
		}
	}
	if !sawExecutePrompt {
		t.Errorf("execute task did not interpolate the triage steps")
	}
}

// TestExample_MarketingBrief runs the marketing preset. Verifies the {{json
// .Stages.brief}} helper produces a JSON-formatted blob in the next prompt
// (the previous string-cast bug would have produced map[key:value] syntax
// that's not valid JSON for downstream parsing).
func TestExample_MarketingBrief(t *testing.T) {
	cfg := loadExample(t, "marketing-brief.json")
	replies := []string{
		// brief stage output
		`{"audience":"indie devs","value_prop":"ship faster","channels":["twitter","blog"],"messages":[{"channel":"twitter","hook":"new release","body":"v1.0 is out"}]}`,
		// review stage output
		`{"approved":true,"notes":"looks good"}`,
		// publish agent finish
		`{"tool":"finish","args":{"message":"published"}}`,
	}
	r, mock, out := newRunner(t, replies)

	tk := ticketFixture()
	tk.Title = "Launch v1.0 announcement"
	tk.Description = "Cross-channel push for the v1.0 launch."

	if _, err := r.RunStages(context.Background(), cfg, tk, "campaign/launch", ""); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if mock.Calls != 3 {
		t.Fatalf("LLM calls = %d; want 3", mock.Calls)
	}

	// The review stage's prompt must contain the brief as actual JSON, not as
	// Go's default map[key:value] string formatting.
	// AllMsgs[1] = the messages sent on the 2nd LLM call (review stage).
	reviewPrompt := mock.AllMsgs[1][len(mock.AllMsgs[1])-1].Content
	if !strings.Contains(reviewPrompt, `"audience":"indie devs"`) {
		t.Errorf("review prompt does not contain JSON-marshaled brief:\n%s", reviewPrompt)
	}
	if strings.Contains(reviewPrompt, "map[") {
		t.Errorf("review prompt fell back to Go's map[] format (json helper did not run):\n%s", reviewPrompt)
	}

	// The publish task should have rendered the ticket key.
	// AllMsgs[2] = the messages sent on the 3rd LLM call (publish agent).
	publishTask := mock.AllMsgs[2][len(mock.AllMsgs[2])-1].Content
	if !strings.Contains(publishTask, "ENG-1") {
		t.Errorf("publish task missing ticket key:\n%s", publishTask)
	}
}

// TestExample_SalesLead runs the sales preset and verifies the conditional
// `if` skip works correctly for cold leads.
func TestExample_SalesLead(t *testing.T) {
	t.Run("hot lead runs all 3 stages", func(t *testing.T) {
		cfg := loadExample(t, "sales-lead.json")
		replies := []string{
			`{"fit":9,"intent":8,"reasoning":"perfect ICP","tier":"hot"}`,
			`{"subject":"hi","body":"intro"}`,
			`{"tool":"finish","args":{"message":"queued"}}`,
		}
		r, mock, _ := newRunner(t, replies)
		tk := ticketFixture()
		tk.Description = "VP Engineering at a 200-person SaaS"
		if _, err := r.RunStages(context.Background(), cfg, tk, "lead/x", ""); err != nil {
			t.Fatalf("run: %v", err)
		}
		if mock.Calls != 3 {
			t.Errorf("hot lead: want 3 LLM calls, got %d", mock.Calls)
		}
	})

	t.Run("cold lead skips draft + push_to_crm", func(t *testing.T) {
		cfg := loadExample(t, "sales-lead.json")
		replies := []string{
			`{"fit":2,"intent":1,"reasoning":"out of ICP","tier":"cold"}`,
		}
		r, mock, _ := newRunner(t, replies)
		if _, err := r.RunStages(context.Background(), cfg, ticketFixture(), "lead/x", ""); err != nil {
			t.Fatalf("run: %v", err)
		}
		if mock.Calls != 1 {
			t.Errorf("cold lead: want 1 LLM call (draft + push_to_crm should skip), got %d", mock.Calls)
		}
	})
}
