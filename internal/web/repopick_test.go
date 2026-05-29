package web

import (
	"fmt"
	"strings"
	"testing"
)

// makeMenu builds a numbered repo-pick menu in the format
// buildRepoGateQuestion emits. Items prefixed with "*" are marked
// suggested.
func makeMenu(items ...string) string {
	var sb strings.Builder
	sb.WriteString("Pick a repo:\n")
	for i, it := range items {
		prefix := "  "
		if strings.HasPrefix(it, "*") {
			prefix = "* "
			it = strings.TrimPrefix(it, "*")
		}
		fmt.Fprintf(&sb, "%s%d. %s\n", prefix, i+1, it)
	}
	return sb.String()
}

func TestRenderRepoPick_DropsBelowTwoOptions(t *testing.T) {
	if got := renderRepoPickButtons(makeMenu("repo-a")); got != "" {
		t.Errorf("expected empty render for <2 options, got: %q", got)
	}
}

func TestRenderRepoPick_BasicTwoOptions(t *testing.T) {
	got := renderRepoPickButtons(makeMenu("repo-a", "repo-b"))
	for _, want := range []string{"repo-a", "repo-b", `data-pick="1"`, `data-pick="2"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output", want)
		}
	}
	if strings.Contains(got, "data-pick-filter") {
		t.Error("small list should not render the filter input")
	}
	if strings.Contains(got, "data-pick-expand") {
		t.Error("small list should not render the expander button")
	}
}

// TestRenderRepoPick_BumpedCap_PastNinetyNine is the regression test
// for the bug where the parser's n>99 cap silently dropped items 100+
// from the UI (orgs with 100+ repos).
func TestRenderRepoPick_BumpedCap_PastNinetyNine(t *testing.T) {
	items := make([]string, 120)
	for i := range items {
		items[i] = fmt.Sprintf("svc-%03d", i+1)
	}
	got := renderRepoPickButtons(makeMenu(items...))
	for _, want := range []string{"svc-100", "svc-120", `data-pick="100"`, `data-pick="120"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q — the n>99 cap should be lifted", want)
		}
	}
}

func TestRenderRepoPick_SortsSuggestedFirst(t *testing.T) {
	// "alpha" comes alphabetically before "beta", but beta is suggested
	// and so should render first in the HTML.
	got := renderRepoPickButtons(makeMenu("alpha", "*beta", "gamma"))
	iBeta := strings.Index(got, ">beta<")
	iAlpha := strings.Index(got, ">alpha<")
	iGamma := strings.Index(got, ">gamma<")
	if iBeta < 0 || iAlpha < 0 || iGamma < 0 {
		t.Fatalf("missing one of beta/alpha/gamma:\n%s", got)
	}
	if !(iBeta < iAlpha && iAlpha < iGamma) {
		t.Errorf("expected order beta < alpha < gamma, got beta=%d alpha=%d gamma=%d", iBeta, iAlpha, iGamma)
	}
}

func TestRenderRepoPick_LargeListAddsFilterAndExpander(t *testing.T) {
	items := make([]string, 30)
	for i := range items {
		items[i] = fmt.Sprintf("svc-%03d", i+1)
	}
	got := renderRepoPickButtons(makeMenu(items...))
	for _, want := range []string{"data-pick-filter", "filter repos by name", "data-pick-expand", "show all 30"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q for large list", want)
		}
	}
}

func TestRenderRepoPick_OverflowMarkedPastBudget(t *testing.T) {
	items := make([]string, 30)
	for i := range items {
		items[i] = fmt.Sprintf("svc-%03d", i+1)
	}
	got := renderRepoPickButtons(makeMenu(items...))
	// initialVisibleOthers = 5 (was 8 pre-UX-pass) → items 6..30 should
	// be overflow-tagged. Cut to keep a 100-repo org from drowning the
	// user's screen in alphabetical noise on first render.
	const initial = 5
	overflowCount := strings.Count(got, `data-overflow="1"`)
	if overflowCount != len(items)-initial {
		t.Errorf("expected %d overflow pills, got %d", len(items)-initial, overflowCount)
	}
}

// --- stripRepoMenu --------------------------------------------------------

func TestStripRepoMenu_RemovesNumberedListAndHeader(t *testing.T) {
	body := `Confirm repo for EB-4978 — "[BE] payload remarks"
Suggested: EB

Available repos:
1. meditap/krakend (remote)
2. meditap/internal-portal-service (remote)
* 3. meditap/dire-provider-service (remote)
4. meditap/identity-iam-service (remote)`
	got := stripRepoMenu(body)
	for _, want := range []string{
		`Confirm repo for EB-4978 — "[BE] payload remarks"`,
		"Suggested: EB",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preamble line %q missing in stripped body:\n%s", want, got)
		}
	}
	for _, gone := range []string{"Available repos:", "meditap/krakend", "meditap/dire-provider-service", "1. ", "* 3."} {
		if strings.Contains(got, gone) {
			t.Errorf("stripped body should not contain %q:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "(picker below — 4 repo options)") {
		t.Errorf("expected picker-count hint, got:\n%s", got)
	}
}

func TestStripRepoMenu_NoMenu_Passthrough(t *testing.T) {
	body := "Approve plan for EB-100?\n\nStep 1: foo\nStep 2: bar"
	if got := stripRepoMenu(body); got != body {
		t.Errorf("body without a repo menu should pass through unchanged.\n got:  %q\n want: %q", got, body)
	}
}

func TestStripRepoMenu_SingletonHint(t *testing.T) {
	body := `Confirm repo for EB-1
Available repos:
1. only/repo`
	got := stripRepoMenu(body)
	if !strings.Contains(got, "1 repo option)") {
		t.Errorf("expected singular 'repo option' for count 1, got:\n%s", got)
	}
}
