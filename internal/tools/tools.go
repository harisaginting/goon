// Package tools holds the Tool interface, the registry, and the strict
// ToolCall JSON schema returned by the LLM.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ToolCall is the strict JSON the LLM must produce.
//
//	{
//	  "tool": "run_command",
//	  "args": { "command": "ls -la" },
//	  "rationale": "list the directory before deciding what to do"
//	}
type ToolCall struct {
	Tool      string            `json:"tool"`
	Args      map[string]string `json:"args"`
	Rationale string            `json:"rationale,omitempty"`
}

// Result is what executing a tool produced.
type Result struct {
	ToolName string
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// Tool is one capability the agent can invoke.
type Tool interface {
	Name() string
	Description() string
	Schema() map[string]string // arg name -> short description
	Run(ctx context.Context, args map[string]string) (Result, error)
}

// Registry holds tools by name.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds a tool. Last registration wins.
func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

// Get returns the tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names returns the tools in stable order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for k := range r.tools {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// All returns all tools sorted by name.
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, n := range r.Names() {
		out = append(out, r.tools[n])
	}
	return out
}

// Manifest renders tool descriptions for the system prompt.
func (r *Registry) Manifest() string {
	var b strings.Builder
	for _, t := range r.All() {
		b.WriteString("- ")
		b.WriteString(t.Name())
		b.WriteString(": ")
		b.WriteString(t.Description())
		if s := t.Schema(); len(s) > 0 {
			b.WriteString(" args=")
			keys := make([]string, 0, len(s))
			for k := range s {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k+"("+s[k]+")")
			}
			b.WriteString(strings.Join(parts, ", "))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// DefaultRegistry returns the built-in tools without memory-bound tools.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(&RunCommand{})
	r.Register(&ReadFile{})
	r.Register(&ListDir{})
	r.Register(&Finish{})
	r.Register(NewConfluenceFromEnv())
	r.Register(NewTelegramFromEnv())
	return r
}

// ParseToolCall extracts the first valid ToolCall from raw model output.
// It tolerates leading/trailing prose, markdown code fences, and extra keys.
// It rejects empty tool names. The returned error is never a *ParseError —
// callers check (err != nil).
func ParseToolCall(raw string) (ToolCall, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ToolCall{}, errors.New("empty model output")
	}
	// Strip ``` fences.
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	// Locate the first balanced JSON object.
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return ToolCall{}, fmt.Errorf("no JSON object found in: %q", trimForLog(raw))
	}
	depth := 0
	end := -1
	inStr := false
	escape := false
	for i := start; i < len(raw); i++ {
		c := raw[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inStr {
			escape = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i + 1
			}
		}
		if end > 0 {
			break
		}
	}
	if end < 0 {
		return ToolCall{}, fmt.Errorf("unterminated JSON object in: %q", trimForLog(raw))
	}
	chunk := raw[start:end]
	var tc ToolCall
	if err := json.Unmarshal([]byte(chunk), &tc); err != nil {
		return ToolCall{}, fmt.Errorf("invalid JSON: %w; payload=%q", err, trimForLog(chunk))
	}
	tc.Tool = strings.TrimSpace(tc.Tool)
	if tc.Tool == "" {
		return ToolCall{}, errors.New(`field "tool" is required`)
	}
	if tc.Args == nil {
		tc.Args = map[string]string{}
	}
	return tc, nil
}

func trimForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
