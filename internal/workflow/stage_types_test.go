package workflow

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/tools"
)

// fakeTool is a network-free tools.Tool used to drive the notify/http stage
// tests. It records the last args it received and returns a canned Result.
type fakeTool struct {
	name     string
	out      string
	err      error
	lastArgs map[string]string
}

func (f *fakeTool) Name() string              { return f.name }
func (f *fakeTool) Description() string       { return "fake " + f.name }
func (f *fakeTool) Schema() map[string]string { return map[string]string{} }
func (f *fakeTool) Run(_ context.Context, args map[string]string) (tools.Result, error) {
	f.lastArgs = args
	if f.err != nil {
		return tools.Result{}, f.err
	}
	return tools.Result{ToolName: f.name, Stdout: f.out}, nil
}

// runnerWith builds a StageRunner whose registry contains exactly the given
// fake tools — so the notify/http stages exercise their real code paths
// without touching the network or a real Telegram bot.
func runnerWith(toolset ...*fakeTool) (*StageRunner, *bytes.Buffer) {
	reg := tools.NewRegistry()
	for _, ft := range toolset {
		reg.Register(ft)
	}
	var out bytes.Buffer
	return &StageRunner{
		Tools:  reg,
		Memory: memory.Disabled(),
		Stdout: &out,
		Stderr: &out,
	}, &out
}

// TestNotifyStage_SendsRenderedMessage verifies the notify stage renders its
// template against ticket/state and hands the result to the 'telegram' tool,
// and that the sent text is stored under .Stages.<name>.
func TestNotifyStage_SendsRenderedMessage(t *testing.T) {
	tele := &fakeTool{name: "telegram"}
	r, _ := runnerWith(tele)

	cfg := WorkflowConfig{Stages: []StageConfig{
		{Name: "ping", Type: StageTypeNotify, Message: "Done with {{.Key}}: {{.Title}}"},
	}}
	state, err := r.RunStages(context.Background(), cfg, ticketFixture(), "b", "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "Done with ENG-1: Add login"
	if tele.lastArgs["text"] != want {
		t.Errorf("telegram text = %q, want %q", tele.lastArgs["text"], want)
	}
	if got, _ := state.Stages["ping"].(string); got != want {
		t.Errorf("stored output = %q, want %q", got, want)
	}
}

// TestNotifyStage_NoToolErrors ensures a notify stage fails loudly (so the
// user knows it's not wired) when no telegram tool is registered.
func TestNotifyStage_NoToolErrors(t *testing.T) {
	r, _ := runnerWith() // empty registry
	cfg := WorkflowConfig{Stages: []StageConfig{
		{Name: "ping", Type: StageTypeNotify, Message: "hi"},
	}}
	_, err := r.RunStages(context.Background(), cfg, ticketFixture(), "b", "")
	if err == nil || !strings.Contains(err.Error(), "telegram") {
		t.Fatalf("expected missing-telegram error, got %v", err)
	}
}

// TestHTTPStage_CapturesBody verifies the http stage renders its URL, calls
// the 'fetch_url' tool, and stores the response body under .Stages.<name>.
func TestHTTPStage_CapturesBody(t *testing.T) {
	fetch := &fakeTool{name: "fetch_url", out: "STATUS OK"}
	r, _ := runnerWith(fetch)

	cfg := WorkflowConfig{Stages: []StageConfig{
		{Name: "status", Type: StageTypeHTTP, URL: "https://example.com/{{.Project}}"},
	}}
	state, err := r.RunStages(context.Background(), cfg, ticketFixture(), "b", "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fetch.lastArgs["url"] != "https://example.com/o/r" {
		t.Errorf("url = %q", fetch.lastArgs["url"])
	}
	if got, _ := state.Stages["status"].(string); got != "STATUS OK" {
		t.Errorf("stored body = %q, want %q", got, "STATUS OK")
	}
}

// TestHTTPStage_ToolErrorPropagates ensures a fetch failure aborts the stage.
func TestHTTPStage_ToolErrorPropagates(t *testing.T) {
	fetch := &fakeTool{name: "fetch_url", err: errors.New("boom")}
	r, _ := runnerWith(fetch)
	cfg := WorkflowConfig{Stages: []StageConfig{
		{Name: "status", Type: StageTypeHTTP, URL: "https://example.com"},
	}}
	_, err := r.RunStages(context.Background(), cfg, ticketFixture(), "b", "")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected fetch error to propagate, got %v", err)
	}
}

// TestValidate_NotifyHTTPRequiredFields locks in the new validation rules
// that handleWorkflowSave (and the daemon) rely on.
func TestValidate_NotifyHTTPRequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		stage   StageConfig
		wantErr bool
	}{
		{"notify ok", StageConfig{Name: "n", Type: StageTypeNotify, Message: "hi"}, false},
		{"notify missing message", StageConfig{Name: "n", Type: StageTypeNotify}, true},
		{"http ok", StageConfig{Name: "h", Type: StageTypeHTTP, URL: "https://x"}, false},
		{"http missing url", StageConfig{Name: "h", Type: StageTypeHTTP}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := WorkflowConfig{Stages: []StageConfig{tc.stage}}.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
