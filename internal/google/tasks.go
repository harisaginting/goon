package google

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// Task is a Google Tasks item flattened for chat.
type Task struct {
	Title  string
	Notes  string
	Due    time.Time
	HasDue bool
	Done   bool
}

type rawTasks struct {
	Items []struct {
		Title  string `json:"title"`
		Notes  string `json:"notes"`
		Due    string `json:"due"`
		Status string `json:"status"` // "needsAction" | "completed"
	} `json:"items"`
}

// TasksList returns the not-completed tasks in the given list (use
// "@default" for the primary list). Sorted by due date (undated last).
func (c *Client) TasksList(ctx context.Context, listID string) ([]Task, error) {
	if strings.TrimSpace(listID) == "" {
		listID = "@default"
	}
	v := url.Values{}
	v.Set("showCompleted", "false")
	v.Set("showHidden", "false")
	v.Set("maxResults", "100")
	u := "https://tasks.googleapis.com/tasks/v1/lists/" + url.PathEscape(listID) + "/tasks?" + v.Encode()
	var raw rawTasks
	if err := c.getJSON(ctx, u, &raw); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(raw.Items))
	for _, it := range raw.Items {
		t := Task{Title: it.Title, Notes: it.Notes, Done: it.Status == "completed"}
		if it.Due != "" {
			if d, err := time.Parse(time.RFC3339, it.Due); err == nil {
				t.Due = d
				t.HasDue = true
			}
		}
		if strings.TrimSpace(t.Title) == "" {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}
