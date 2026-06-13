package workflow

import (
	"embed"
	"encoding/json"
	"fmt"
)

// templateFS holds the starter workflow scaffolds offered by the web
// pipeline editor's "start from template" dropdown. They mirror the
// user-facing examples/ library at the repo root but are embedded here so
// the web layer can serve them without a filesystem lookup (the binary
// might run anywhere). Keep these in sync with examples/workflows/ when the
// canonical scaffolds change.
//
//go:embed templates/*.json
var templateFS embed.FS

// StarterTemplate is a named, ready-to-load workflow scaffold. Key is the
// stable identifier used by the editor's dropdown; Label/Desc are shown to
// the user; Config is the parsed scaffold the editor seeds from.
type StarterTemplate struct {
	Key    string         `json:"key"`
	Label  string         `json:"label"`
	Desc   string         `json:"desc"`
	Config WorkflowConfig `json:"config"`
}

// starterMeta gives each embedded template a human label + ordering. The
// first entry is the canonical "built-in pipeline as editable stages" seed
// used when a user edits the default pipeline.
var starterMeta = []struct {
	file, key, label string
}{
	{"templates/default-stages.json", "default", "Built-in pipeline (editable stages)"},
	{"templates/minimal.json", "minimal", "Minimal (inherit all defaults)"},
	{"templates/marketing-brief.json", "marketing-brief", "Marketing brief → review → publish"},
	{"templates/sales-lead.json", "sales-lead", "Sales lead qualify → draft → CRM"},
}

// StarterTemplates parses and returns the embedded scaffolds in display
// order. A parse error in any embedded file is a build-time mistake, so it
// is returned rather than silently skipped — callers can log-and-degrade.
func StarterTemplates() ([]StarterTemplate, error) {
	out := make([]StarterTemplate, 0, len(starterMeta))
	for _, m := range starterMeta {
		b, err := templateFS.ReadFile(m.file)
		if err != nil {
			return nil, fmt.Errorf("read template %s: %w", m.file, err)
		}
		var cfg WorkflowConfig
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("parse template %s: %w", m.file, err)
		}
		out = append(out, StarterTemplate{
			Key:    m.key,
			Label:  m.label,
			Desc:   cfg.Description,
			Config: cfg,
		})
	}
	return out, nil
}

// BuiltinStageSeed returns the declarative-stage representation of the
// built-in pipeline (the "default" starter). The web editor seeds from this
// when the user opens the editor on a config that has no stages of its own,
// so "edit pipeline" shows the real current pipeline instead of a blank
// canvas. Returns nil if the embedded seed is unavailable (should never
// happen — it's compiled in).
func BuiltinStageSeed() []StageConfig {
	// BuiltinRoleGraph builds a fresh slice on every call, so the editor can
	// freely mutate the returned seed without corrupting anything cached.
	return BuiltinRoleGraph()
}

// AutomationStarters returns ready-to-save scheduled-automation scaffolds for
// the "new automation" picker. Each is a small executor → notify graph on a
// sensible default schedule; the user edits the task, message, and schedule.
func AutomationStarters() []StarterTemplate {
	enabled := true
	mk := func(key, label, desc string, trig Trigger, stages []StageConfig) StarterTemplate {
		return StarterTemplate{Key: key, Label: label, Desc: desc, Config: WorkflowConfig{
			Version: 2, Name: label, Description: desc,
			Trigger: trig, Enabled: &enabled, Stages: stages,
		}}
	}
	return []StarterTemplate{
		mk("health-check", "Service health check",
			"Run health checks on a schedule and alert on failure.",
			Trigger{Type: "schedule", Every: "15m"},
			[]StageConfig{
				{Name: "check", Type: RoleExecutor, OnNext: StringList{"report"},
					Task: "Run the project's health checks: curl/HTTP the key endpoints (or run the health script) and inspect the results. Summarise what is UP and what is DOWN in a few lines; if everything is healthy, say so briefly."},
				{Name: "report", Type: RoleNotify,
					Message: "🩺 Health check\n{{.Stages.check}}"},
			}),
		mk("standup-digest", "Daily standup digest",
			"Summarise recent activity into a short standup each weekday morning.",
			Trigger{Type: "schedule", Cron: "0 9 * * 1-5"},
			[]StageConfig{
				{Name: "gather", Type: RoleExecutor, OnNext: StringList{"post"},
					Task: "Produce a short daily standup: what shipped recently and what is in progress. Use run_command to read recent git log across the repos and check goon's HISTORY.md / memory notes. Reply with 5-8 plain, skimmable bullet lines."},
				{Name: "post", Type: RoleNotify,
					Message: "☀️ Standup\n{{.Stages.gather}}"},
			}),
		mk("email-digest", "Email digest",
			"Read recent email on a schedule, summarise it, and notify. Needs a mail tool/CLI available to goon's agent.",
			Trigger{Type: "schedule", Cron: "0 8 * * *"},
			[]StageConfig{
				{Name: "read", Type: RoleExecutor, OnNext: StringList{"digest"},
					Task: "Fetch recent/unread email (via the configured mail tool or CLI) and summarise senders, subjects, and anything that needs a reply. Keep it tight. If no mail tool is available, say so plainly."},
				{Name: "digest", Type: RoleNotify,
					Message: "📬 Email digest\n{{.Stages.read}}"},
			}),
	}
}

// Validate checks a workflow config for problems that would make the daemon
// skip it or crash mid-run. It is the single validation entry point shared
// by the web save handler and any future CLI lint command, so what the
// editor accepts is exactly what the daemon will accept on the next poll.
//
// Currently it validates the declarative stage list (unique names, valid
// types, required per-type fields). Workflow-level fields all have permissive
// defaults and don't need validation here.
func (c WorkflowConfig) Validate() error {
	if len(c.Stages) > 0 {
		if err := validateStages(c.Stages); err != nil {
			return err
		}
	}
	return nil
}
