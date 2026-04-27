package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"goon/internal/memory"
)

// parseTriage extracts a plan + repo from the LLM's triage reply.
//
// Schema: {"steps":[{"title":"..."}, ...], "repo":"path"}
func parseTriage(raw string) ([]memory.PlanStep, string, error) {
	chunk, err := extractJSONObject(raw)
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		Steps []struct {
			Title string `json:"title"`
		} `json:"steps"`
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(chunk), &resp); err != nil {
		return nil, "", fmt.Errorf("invalid JSON: %w", err)
	}
	if len(resp.Steps) == 0 {
		return nil, "", errors.New(`"steps" array is empty`)
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
		return nil, "", errors.New("no usable steps")
	}
	return plan, strings.TrimSpace(resp.Repo), nil
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
