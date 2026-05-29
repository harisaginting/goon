package review

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
)

// testMemory returns a Memory backed by a throwaway temp file.
func testMemory(t *testing.T) *memory.Memory {
	t.Helper()
	m, err := memory.New(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	return m
}

// reviewHost builds a Mock pre-seeded so PendingReviews can resolve a
// single review-requested PR plus its diff. The PR must live in BOTH
// ReviewPRs (for ReviewRequestedPRs) and OpenPRs (for GetPRDetails).
func reviewHost(diff string) *githost.Mock {
	m := githost.NewMock()
	pr := githost.PR{Number: 1, Repo: "o/r", Title: "Add cache", URL: "https://x/pr/1", Author: "alice"}
	m.ReviewPRs = []githost.PR{pr}
	m.OpenPRs = []githost.PR{pr}
	m.Diffs[1] = diff
	return m
}

func TestRunner_PendingReviews_DraftsAndDedups(t *testing.T) {
	host := reviewHost("diff --git a/x b/x\n+one\n")
	mem := testMemory(t)
	mockLLM := llm.NewMock([]string{"SUMMARY — adds a cache layer."})
	r := New(Options{Host: host, LLM: mockLLM, Memory: mem})

	drafts, err := r.PendingReviews(context.Background(), false)
	if err != nil {
		t.Fatalf("PendingReviews: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("want 1 draft, got %d", len(drafts))
	}
	if drafts[0].Repo != "o/r" || drafts[0].Number != 1 {
		t.Errorf("draft target: %+v", drafts[0])
	}
	if !strings.Contains(drafts[0].Body, "cache layer") {
		t.Errorf("draft body: %q", drafts[0].Body)
	}
	if drafts[0].DiffHash == "" {
		t.Error("draft DiffHash should be set")
	}
	r.MarkReviewed(drafts[0])

	// Unchanged diff → dedup skips the PR; the LLM is not consulted.
	mockLLM.Replies = []string{"UNEXPECTED"}
	drafts, err = r.PendingReviews(context.Background(), false)
	if err != nil {
		t.Fatalf("PendingReviews (dedup): %v", err)
	}
	if len(drafts) != 0 {
		t.Fatalf("expected dedup to skip the PR, got %d drafts", len(drafts))
	}

	// Diff changes → re-review.
	host.Diffs[1] = "diff --git a/x b/x\n+two\n"
	mockLLM.Replies = []string{"SUMMARY — changed implementation."}
	drafts, err = r.PendingReviews(context.Background(), false)
	if err != nil {
		t.Fatalf("PendingReviews (re-review): %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("expected re-review after diff change, got %d", len(drafts))
	}
}

func TestRunner_PendingReviews_IgnoreDedup(t *testing.T) {
	host := reviewHost("diff x")
	mem := testMemory(t)
	mockLLM := llm.NewMock([]string{"r1", "r2"})
	r := New(Options{Host: host, LLM: mockLLM, Memory: mem})

	d, err := r.PendingReviews(context.Background(), true)
	if err != nil || len(d) != 1 {
		t.Fatalf("first pass: err=%v len=%d", err, len(d))
	}
	r.MarkReviewed(d[0])

	// ignoreDedup=true → drafts again even though nothing changed.
	d, err = r.PendingReviews(context.Background(), true)
	if err != nil || len(d) != 1 {
		t.Fatalf("ignoreDedup pass: err=%v len=%d", err, len(d))
	}
}

func TestRunner_PendingReviews_UnsupportedHost(t *testing.T) {
	r := New(Options{Host: bareHost{}, LLM: llm.NewMock(nil)})
	_, err := r.PendingReviews(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "review requests") {
		t.Fatalf("expected unsupported-host error, got %v", err)
	}
}

func TestRunner_Notifications_SingleAndDedup(t *testing.T) {
	host := githost.NewMock()
	host.Notifs = []githost.Notification{
		{ID: "n1", Kind: "review_requested", Title: "Review me", Repo: "o/r"},
	}
	mem := testMemory(t)
	r := New(Options{Host: host, LLM: llm.NewMock(nil), Memory: mem})

	batch, err := r.Notifications(context.Background(), false)
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(batch.Items))
	}
	if batch.Summary != "" {
		t.Errorf("a single item should carry no digest, got %q", batch.Summary)
	}
	r.MarkNotified(batch)

	batch, err = r.Notifications(context.Background(), false)
	if err != nil {
		t.Fatalf("Notifications (dedup): %v", err)
	}
	if len(batch.Items) != 0 {
		t.Fatalf("expected dedup to drop the seen item, got %d", len(batch.Items))
	}
}

func TestRunner_Notifications_MultipleGetsDigest(t *testing.T) {
	host := githost.NewMock()
	host.Notifs = []githost.Notification{
		{ID: "n1", Kind: "review_requested", Title: "PR A", Repo: "o/a"},
		{ID: "n2", Kind: "mention", Title: "Issue B", Repo: "o/b"},
		{ID: "n3", Kind: "other", Title: "ignored", Repo: "o/c"},
	}
	mem := testMemory(t)
	r := New(Options{Host: host, LLM: llm.NewMock([]string{"Two items need attention."}), Memory: mem})

	batch, err := r.Notifications(context.Background(), false)
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	if len(batch.Items) != 2 {
		t.Fatalf("want 2 actionable items (kind=other filtered out), got %d", len(batch.Items))
	}
	if batch.Summary != "Two items need attention." {
		t.Errorf("digest: %q", batch.Summary)
	}
}

func TestRunner_Notifications_UnsupportedHost(t *testing.T) {
	r := New(Options{Host: bareHost{}, LLM: llm.NewMock(nil)})
	_, err := r.Notifications(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "notification") {
		t.Fatalf("expected unsupported-host error, got %v", err)
	}
}

func TestFormatDraftText(t *testing.T) {
	out := FormatDraftText(Draft{Repo: "o/r", Number: 5, Title: "T", URL: "u", Body: "the review"})
	for _, want := range []string{"o/r#5", "T", "u", "the review"} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatDraftText missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatNotifText(t *testing.T) {
	if FormatNotifText(NotifBatch{}) != "" {
		t.Error("empty batch should format to an empty string")
	}
	out := FormatNotifText(NotifBatch{
		Items: []githost.Notification{
			{Kind: "review_requested", Title: "PR A", Repo: "o/a"},
			{Kind: "mention", Title: "Issue B", Repo: "o/b"},
		},
		Summary: "digest line",
	})
	for _, want := range []string{"2 new", "digest line", "PR A", "Issue B", "review request"} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatNotifText missing %q in:\n%s", want, out)
		}
	}
}

// bareHost implements only githost.Host — no ReviewRequester / Notifier —
// so it exercises the graceful-degradation error paths.
type bareHost struct{}

func (bareHost) Name() string { return "bare" }
func (bareHost) OpenPR(_ context.Context, _ githost.CreateOptions) (githost.PR, error) {
	return githost.PR{}, nil
}

// --- trimDiffSmart -------------------------------------------------------

func TestTrimDiffSmart_Passthrough(t *testing.T) {
	small := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n"
	if out := trimDiffSmart(small, 10000); out != small {
		t.Errorf("small diff should pass through unchanged, got: %q", out)
	}
}

// TestTrimDiffSmart_LargeYieldsDigest is the regression test for the
// 784 KB PR case: the LLM must see EVERY file in the change set, not
// just the first 18 KB worth.
func TestTrimDiffSmart_LargeYieldsDigest(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&sb, "diff --git a/pkg/file%d.go b/pkg/file%d.go\n", i, i)
		fmt.Fprintf(&sb, "--- a/pkg/file%d.go\n+++ b/pkg/file%d.go\n", i, i)
		sb.WriteString("@@ -1,100 +1,100 @@\n")
		for j := 0; j < 200; j++ {
			fmt.Fprintf(&sb, "-old line %d\n+new line %d\n", j, j)
		}
	}
	raw := sb.String()
	const max = 4000
	out := trimDiffSmart(raw, max)
	if !strings.Contains(out, "DIFF DIGEST") {
		t.Errorf("expected digest header in:\n%s", out)
	}
	if !strings.Contains(out, "Files changed:") {
		t.Errorf("expected file list header")
	}
	// The win: even when excerpts get trimmed, the file list always
	// shows all 5 files so the LLM knows the SHAPE of the PR.
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("pkg/file%d.go", i)
		if !strings.Contains(out, want) {
			t.Errorf("file list missing %q in:\n%s", want, out)
		}
	}
	// Size should stay reasonably bounded (not exact — per-file
	// excerpts can overshoot slightly).
	if len(out) > 2*max {
		t.Errorf("digest much larger than budget: %d bytes (cap was %d)", len(out), max)
	}
}

func TestTrimDiffSmart_MalformedFallsBack(t *testing.T) {
	plain := strings.Repeat("some text without diff markers\n", 200)
	out := trimDiffSmart(plain, 100)
	if strings.Contains(out, "DIFF DIGEST") {
		t.Errorf("malformed diff should not produce a digest, got:\n%s", out)
	}
	if !strings.Contains(out, "diff truncated") {
		t.Errorf("expected fallback truncation marker, got: %q", out)
	}
}

func TestSplitDiffByFile(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@\n+x\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@\n+y\n"
	files := splitDiffByFile(diff)
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].path != "a.go" || files[1].path != "b.go" {
		t.Errorf("paths: %q, %q", files[0].path, files[1].path)
	}
	if !strings.Contains(files[0].body, "+x") {
		t.Errorf("file 0 body: %q", files[0].body)
	}
	if !strings.Contains(files[1].body, "+y") {
		t.Errorf("file 1 body: %q", files[1].body)
	}
}

func TestSplitDiffByFile_NoMarkers(t *testing.T) {
	if files := splitDiffByFile("just some plain text"); files != nil {
		t.Errorf("expected nil for input without diff markers, got %v", files)
	}
}

func TestCountAddsDels(t *testing.T) {
	body := "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n-old\n-removed\n+new\n+added\n+also\n context\n"
	adds, dels := countAddsDels(body)
	if adds != 3 || dels != 2 {
		t.Errorf("counts: +%d -%d, want +3 -2", adds, dels)
	}
}

func TestExtractDiffPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"diff --git a/foo/bar.go b/foo/bar.go", "foo/bar.go"},
		{"diff --git a/x b/y/z", "y/z"}, // renamed
	}
	for _, c := range cases {
		if got := extractDiffPath(c.in); got != c.want {
			t.Errorf("extractDiffPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
