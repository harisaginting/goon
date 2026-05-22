package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/harisaginting/goon/internal/memory"
)

// TriageResult is the structured outcome of one triage LLM call.
// Bundles the legacy "plan + primary repo" return with the new
// signals — needs_repo classification and multi-repo suggestions —
// so adding fields here doesn't force every call site to grow new
// positional return values.
type TriageResult struct {
	Plan      []memory.PlanStep
	Repo      string   // primary suggestion (may be a name from REPOSITORY.md)
	Repos     []string // every suggested repo when the LLM picked >1
	NeedsRepo bool     // false → ticket is research/docs/comms, skip the repo gate
}

// parseTriage extracts plan + repo + needs_repo from the LLM's
// triage reply.
//
// Schema (current):
//
//	{
//	  "steps":      [{"title": "…"}, …],
//	  "repo":       "name-or-path",     // optional primary pick
//	  "repos":      ["a", "b"],          // optional multi-pick
//	  "needs_repo": true                 // optional, defaults to true
//	}
//
// Backwards-compat: a reply with only `steps` + `repo` still works
// and is treated as needs_repo=true (the historical default before
// this classification existed).
func parseTriage(raw string) (TriageResult, error) {
	chunk, err := extractJSONObject(raw)
	if err != nil {
		return TriageResult{}, err
	}
	// needsRepoPtr is a *bool so we can distinguish "field absent"
	// (legacy reply) from "field present and false" (modern reply
	// classifying this as a non-code ticket).
	var resp struct {
		Steps []struct {
			Title string `json:"title"`
		} `json:"steps"`
		Repo      string   `json:"repo"`
		Repos     []string `json:"repos"`
		NeedsRepo *bool    `json:"needs_repo"`
	}
	if err := json.Unmarshal([]byte(chunk), &resp); err != nil {
		return TriageResult{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(resp.Steps) == 0 {
		return TriageResult{}, errors.New(`"steps" array is empty`)
	}
	plan := make([]memory.PlanStep, 0, len(resp.Steps))
	for i, s := range resp.Steps {
		title := strings.TrimSpace(s.Title)
		if title == "" {
			continue
		}
		plan = append(plan, memory.PlanStep{Index: i, Title: title})
	}
	if len(plan) == 0 {
		return TriageResult{}, errors.New("no usable steps")
	}
	// Merge repo + repos. If both are set we trust `repos` for the
	// full list but keep `repo` as the primary (deduped + first).
	repos := dedupeTrim(resp.Repos)
	primary := strings.TrimSpace(resp.Repo)
	if primary == "" && len(repos) > 0 {
		primary = repos[0]
	}
	if primary != "" {
		// Ensure primary is in the repos slice and at index 0.
		out := []string{primary}
		for _, r := range repos {
			if r != primary {
				out = append(out, r)
			}
		}
		repos = out
	}
	// Default-on for needs_repo so older prompts (and providers that
	// drop unknown fields) keep behaving like before. Only an
	// explicit `"needs_repo": false` flips it off.
	needs := true
	if resp.NeedsRepo != nil {
		needs = *resp.NeedsRepo
	}
	// Edge case: the model said "no repo needed" but also returned
	// repo suggestions. Trust the boolean — but log nothing because
	// this function is import-clean (no logger dep). Caller may
	// inspect both fields.
	return TriageResult{
		Plan:      plan,
		Repo:      primary,
		Repos:     repos,
		NeedsRepo: needs,
	}, nil
}

// dedupeTrim trims every string + drops empties + dupes, preserving
// first-seen order.
func dedupeTrim(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// extractJSONObject finds the first balanced JSON object inside arbitrary
// text (tolerates code fences and prose). Mirrors tools.ParseToolCall.
func extractJSONObject(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty")
	}
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return "", errors.New("no JSON object")
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
		return "", errors.New("unterminated JSON")
	}
	return raw[start:end], nil
}
