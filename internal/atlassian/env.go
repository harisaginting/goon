// Package atlassian centralizes the env-var resolution for Atlassian Cloud
// (Jira + Confluence). Both products share an Atlassian Account, so the same
// email and API token usually covers both — there's no reason to type them
// twice in your .env.
//
// Resolution rules (in priority order):
//
//  1. The product-specific var (JIRA_BASE_URL, CONFLUENCE_EMAIL, ...) wins
//     when set. This preserves backwards compatibility AND lets self-hosted
//     installs point each product at a different host.
//  2. ATLASSIAN_BASE_URL / ATLASSIAN_EMAIL / ATLASSIAN_API_TOKEN fill in the
//     gaps. For Confluence specifically, when only the shared base URL is set
//     and it does NOT already end in "/wiki", the helper appends "/wiki" so
//     the most common Atlassian Cloud layout works without further config.
//
// Example minimal cloud setup (.env):
//
//	ATLASSIAN_BASE_URL=https://acme.atlassian.net
//	ATLASSIAN_EMAIL=you@acme.com
//	ATLASSIAN_API_TOKEN=...
//
// That single set covers Jira and Confluence. Override per-product only when
// the URL or auth genuinely differs (Data Center splits, separate accounts).
package atlassian

import (
	"os"
	"strings"
)

// Creds holds the resolved (BaseURL, Email, APIToken) for a single product.
type Creds struct {
	BaseURL  string
	Email    string
	APIToken string
}

// Filled reports whether all three fields are non-empty.
func (c Creds) Filled() bool {
	return c.BaseURL != "" && c.Email != "" && c.APIToken != ""
}

// Jira returns Jira credentials, falling back to the ATLASSIAN_* shared vars.
func Jira() Creds {
	return Creds{
		BaseURL:  trimSlash(firstNonEmpty(os.Getenv("JIRA_BASE_URL"), os.Getenv("ATLASSIAN_BASE_URL"))),
		Email:    firstNonEmpty(os.Getenv("JIRA_EMAIL"), os.Getenv("ATLASSIAN_EMAIL")),
		APIToken: firstNonEmpty(os.Getenv("JIRA_API_TOKEN"), os.Getenv("ATLASSIAN_API_TOKEN")),
	}
}

// Confluence returns Confluence credentials, falling back to the ATLASSIAN_*
// shared vars. When the base URL came from the shared var and does NOT
// already end in "/wiki", "/wiki" is appended (the standard Cloud layout).
func Confluence() Creds {
	c := Creds{
		Email:    firstNonEmpty(os.Getenv("CONFLUENCE_EMAIL"), os.Getenv("ATLASSIAN_EMAIL")),
		APIToken: firstNonEmpty(os.Getenv("CONFLUENCE_API_TOKEN"), os.Getenv("ATLASSIAN_API_TOKEN")),
	}
	if v := strings.TrimSpace(os.Getenv("CONFLUENCE_BASE_URL")); v != "" {
		c.BaseURL = trimSlash(v)
		return c
	}
	if v := strings.TrimSpace(os.Getenv("ATLASSIAN_BASE_URL")); v != "" {
		base := trimSlash(v)
		if !strings.HasSuffix(base, "/wiki") {
			base += "/wiki"
		}
		c.BaseURL = base
	}
	return c
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func trimSlash(s string) string { return strings.TrimRight(s, "/") }
