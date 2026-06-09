// Package agent implements the multi-step agent loop.
//
//	for step := 0; step < MaxSteps; step++ {
//	    1. snapshot context (cwd, files, last output, memory)
//	    2. ask LLM for one ToolCall (strict JSON)
//	    3. parse + validate
//	    4. execute via executor (mode-aware, safety-checked)
//	    5. append result to chat history; loop
//	    6. stop when tool == "finish"
//	}
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/tools"
)

// MaxSteps is the hard upper bound on agent turns. Override with
// GOON_MAX_STEPS at runtime.
var MaxSteps = func() int {
	if v := os.Getenv("GOON_MAX_STEPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			return n
		}
	}
	return 5
}()

// Options groups dependencies passed to New.
type Options struct {
	LLM      llm.Provider
	Tools    *tools.Registry
	Executor *executor.Executor
	Memory   *memory.Memory
	Stdout   io.Writer
	Stderr   io.Writer
	Debug    bool
}

// Agent runs the multi-step loop.
type Agent struct {
	opts   Options
	result string // the final "finish" message, exposed via Result()
}

// New constructs an Agent.
func New(opts Options) *Agent { return &Agent{opts: opts} }

// Result returns the agent's final "finish" message (the task outcome /
// answer), available after Run returns. Empty if the agent never called
// finish. Lets callers surface the result instead of discarding it.
func (a *Agent) Result() string { return a.result }

// Run executes the loop and returns the final summary or the last error.
func (a *Agent) Run(ctx context.Context, task string) error {
	if a.opts.LLM == nil {
		return errors.New("agent: missing LLM provider")
	}
	if a.opts.Tools == nil {
		return errors.New("agent: missing tool registry")
	}
	if a.opts.Executor == nil {
		return errors.New("agent: missing executor")
	}

	system := SystemPrompt(a.opts.Tools)
	chat := []llm.Message{{Role: llm.RoleSystem, Content: system}}

	var lastOutput string
	var lastErr error

	for step := 0; step < MaxSteps; step++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		snapshot := Snapshot(tools.WorkDirFrom(ctx), lastOutput, a.opts.Memory.FrequentCommands(5))
		userMsg := BuildUserContext(task, snapshot)
		if step == 0 {
			chat = append(chat, llm.Message{Role: llm.RoleUser, Content: userMsg})
		} else {
			// Subsequent turns: surface the last result back to the model.
			chat = append(chat, llm.Message{
				Role:    llm.RoleUser,
				Content: "ENVIRONMENT UPDATE:\n" + snapshot.Render(),
			})
		}

		logx.Debug("agent.prompt", "step", step, "provider", a.opts.LLM.Name(),
			"messages", len(chat), "task", task)
		raw, err := a.opts.LLM.Generate(ctx, chat, llm.Options{
			Temperature: 0.1,
			MaxTokens:   1024,
			JSONMode:    true,
		})
		if err != nil {
			logx.Error("agent.llm_error", "step", step, "err", err.Error())
			return fmt.Errorf("llm: %w", err)
		}
		logx.Debug("agent.response", "step", step, "raw_bytes", len(raw),
			"raw", logTruncate(raw, 4096))
		if a.opts.Debug {
			fmt.Fprintf(a.opts.Stderr, "[debug] step=%d raw=%s\n", step, raw)
		}

		// Persist assistant turn.
		chat = append(chat, llm.Message{Role: llm.RoleAssistant, Content: raw})

		call, err := tools.ParseToolCall(raw)
		if err != nil {
			// Self-correction: feed back the parse error.
			chat = append(chat, llm.Message{
				Role: llm.RoleUser,
				Content: "ERROR: your previous output was not valid JSON.\n" +
					"Reason: " + err.Error() + "\n" +
					"Reply with EXACTLY ONE JSON object matching the schema.",
			})
			continue
		}

		// Look up tool.
		tool, ok := a.opts.Tools.Get(call.Tool)
		if !ok {
			chat = append(chat, llm.Message{
				Role: llm.RoleUser,
				Content: fmt.Sprintf(
					"ERROR: tool %q is not registered. Choose one of: %s.",
					call.Tool, strings.Join(a.opts.Tools.Names(), ", ")),
			})
			continue
		}

		// Print intent before executing.
		fmt.Fprintf(a.opts.Stdout, "→ step %d/%d  tool=%s  args=%v\n",
			step+1, MaxSteps, call.Tool, call.Args)
		if call.Rationale != "" && a.opts.Debug {
			fmt.Fprintf(a.opts.Stderr, "  [rationale] %s\n", call.Rationale)
		}
		logx.Info("agent.tool_call", "step", step, "tool", call.Tool,
			"args", call.Args, "rationale", call.Rationale)

		// Finish short-circuits.
		if call.Tool == "finish" {
			msg := call.Args["message"]
			if msg == "" {
				msg = "done"
			}
			a.result = msg
			fmt.Fprintln(a.opts.Stdout, msg)
			a.opts.Memory.Append(memory.Interaction{
				Input: task, ToolUsed: "finish", OK: true, Output: msg,
			})
			return nil
		}

		res, runErr := a.opts.Executor.Execute(ctx, tool, call)
		lastErr = runErr

		// Render result (truncated).
		printResult(a.opts.Stdout, res, runErr)

		// Append tool result for the next LLM turn.
		chat = append(chat, llm.Message{
			Role:    llm.RoleTool,
			Content: formatToolResultForLLM(call.Tool, res, runErr),
		})

		// Memory: record the interaction.
		a.opts.Memory.Append(memory.Interaction{
			Input:    task,
			ToolUsed: call.Tool,
			Command:  call.Args["command"],
			OK:       runErr == nil,
			Output:   truncateForMem(res.Stdout, 800),
		})

		// Self-healing: if a run_command failed, prepend a heal hint.
		if runErr != nil && call.Tool == "run_command" {
			chat = append(chat, llm.Message{
				Role: llm.RoleUser,
				Content: "Previous command failed. Stderr:\n" +
					truncateLines(res.Stderr, 30) +
					"\nFix it on the next step or call finish if not recoverable.",
			})
		}

		lastOutput = strings.TrimSpace(res.Stdout)
	}

	if lastErr != nil {
		return fmt.Errorf("max steps reached; last error: %w", lastErr)
	}
	fmt.Fprintln(a.opts.Stdout, "(max steps reached without finish)")
	return nil
}

func formatToolResultForLLM(name string, r tools.Result, runErr error) string {
	// XML delimiters prevent tool output (e.g. file contents or command
	// stdout) from spoofing a previous TOOL RESULT block or injecting
	// instructions by making the boundary explicit and unambiguous.
	var b strings.Builder
	fmt.Fprintf(&b, "<tool_result name=%q>\n", name)
	if runErr != nil {
		fmt.Fprintf(&b, "error: %s\n", runErr.Error())
	}
	if r.ExitCode != 0 {
		fmt.Fprintf(&b, "exit_code: %d\n", r.ExitCode)
	}
	if r.Stdout != "" {
		fmt.Fprintf(&b, "stdout:\n%s\n", truncateLines(r.Stdout, 30))
	}
	if r.Stderr != "" {
		fmt.Fprintf(&b, "stderr:\n%s\n", truncateLines(r.Stderr, 30))
	}
	b.WriteString("</tool_result>\n")
	return b.String()
}

func printResult(out io.Writer, r tools.Result, runErr error) {
	if r.Stdout != "" {
		fmt.Fprintln(out, truncateLines(r.Stdout, 30))
	}
	if r.Stderr != "" {
		fmt.Fprintf(out, "[stderr] %s\n", truncateLines(r.Stderr, 10))
	}
	if runErr != nil {
		fmt.Fprintf(out, "[error] %s\n", runErr.Error())
	}
}

func truncateForMem(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// logTruncate is the version used in structured-log attributes; identical
// to truncateForMem but expressed separately so the intent at call sites
// is obvious ("this is for the log").
func logTruncate(s string, max int) string { return truncateForMem(s, max) }
