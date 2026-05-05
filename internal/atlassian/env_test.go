package atlassian

import "testing"

func clearAll(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ATLASSIAN_BASE_URL", "ATLASSIAN_EMAIL", "ATLASSIAN_API_TOKEN",
		"JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_API_TOKEN",
		"CONFLUENCE_BASE_URL", "CONFLUENCE_EMAIL", "CONFLUENCE_API_TOKEN",
	} {
		t.Setenv(k, "")
	}
}

func TestJira_FromSharedOnly(t *testing.T) {
	clearAll(t)
	t.Setenv("ATLASSIAN_BASE_URL", "https://acme.atlassian.net/")
	t.Setenv("ATLASSIAN_EMAIL", "you@acme.com")
	t.Setenv("ATLASSIAN_API_TOKEN", "tok-123")

	got := Jira()
	want := Creds{BaseURL: "https://acme.atlassian.net", Email: "you@acme.com", APIToken: "tok-123"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !got.Filled() {
		t.Error("Filled() returned false")
	}
}

func TestJira_PerProductWinsOverShared(t *testing.T) {
	clearAll(t)
	t.Setenv("ATLASSIAN_BASE_URL", "https://shared.atlassian.net")
	t.Setenv("ATLASSIAN_EMAIL", "shared@x.com")
	t.Setenv("ATLASSIAN_API_TOKEN", "shared-tok")

	t.Setenv("JIRA_BASE_URL", "https://jira.self-hosted.com")
	t.Setenv("JIRA_EMAIL", "jira@x.com")
	t.Setenv("JIRA_API_TOKEN", "jira-tok")

	got := Jira()
	want := Creds{BaseURL: "https://jira.self-hosted.com", Email: "jira@x.com", APIToken: "jira-tok"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestConfluence_FromSharedAutoAppendsWiki(t *testing.T) {
	clearAll(t)
	t.Setenv("ATLASSIAN_BASE_URL", "https://acme.atlassian.net")
	t.Setenv("ATLASSIAN_EMAIL", "you@acme.com")
	t.Setenv("ATLASSIAN_API_TOKEN", "tok-123")

	got := Confluence()
	want := Creds{BaseURL: "https://acme.atlassian.net/wiki", Email: "you@acme.com", APIToken: "tok-123"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestConfluence_SharedAlreadyHasWiki_DoesNotDoubleAppend(t *testing.T) {
	clearAll(t)
	t.Setenv("ATLASSIAN_BASE_URL", "https://acme.atlassian.net/wiki")
	t.Setenv("ATLASSIAN_EMAIL", "you@acme.com")
	t.Setenv("ATLASSIAN_API_TOKEN", "tok")

	got := Confluence()
	if got.BaseURL != "https://acme.atlassian.net/wiki" {
		t.Errorf("base url got %q; want no double-append", got.BaseURL)
	}
}

func TestConfluence_PerProductWins(t *testing.T) {
	clearAll(t)
	t.Setenv("ATLASSIAN_BASE_URL", "https://acme.atlassian.net")
	t.Setenv("ATLASSIAN_EMAIL", "shared@x.com")
	t.Setenv("CONFLUENCE_BASE_URL", "https://wiki.self-hosted.com/")
	t.Setenv("CONFLUENCE_EMAIL", "wiki@x.com")
	t.Setenv("CONFLUENCE_API_TOKEN", "wiki-tok")

	got := Confluence()
	want := Creds{BaseURL: "https://wiki.self-hosted.com", Email: "wiki@x.com", APIToken: "wiki-tok"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestConfluence_PerProductBaseDoesNotGetWikiAppended(t *testing.T) {
	// If the user explicitly set CONFLUENCE_BASE_URL=https://wiki.x.com they
	// don't want us appending /wiki to it. Only the shared-base auto-magic
	// does the append.
	clearAll(t)
	t.Setenv("CONFLUENCE_BASE_URL", "https://wiki.self-hosted.com")
	t.Setenv("CONFLUENCE_EMAIL", "you@x.com")
	t.Setenv("CONFLUENCE_API_TOKEN", "tok")

	got := Confluence()
	if got.BaseURL != "https://wiki.self-hosted.com" {
		t.Errorf("base url got %q; per-product var should not be modified", got.BaseURL)
	}
}

func TestNotConfigured(t *testing.T) {
	clearAll(t)
	if Jira().Filled() {
		t.Error("Jira().Filled() should be false when nothing is set")
	}
	if Confluence().Filled() {
		t.Error("Confluence().Filled() should be false when nothing is set")
	}
}
