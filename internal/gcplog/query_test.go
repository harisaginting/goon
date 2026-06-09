package gcplog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuild_Trace(t *testing.T) {
	f := Build("", "abc123", "", "", 24)
	if !strings.Contains(f, `trace:"abc123"`) {
		t.Fatalf("trace filter missing: %q", f)
	}
	if !strings.Contains(f, "timestamp>=") {
		t.Fatalf("time window missing: %q", f)
	}
	// When a trace is given, user/query text must not also be ANDed in.
	if strings.Count(f, "AND") != 1 {
		t.Fatalf("trace path should be trace AND time only: %q", f)
	}
}

func TestBuild_UserAndQuery(t *testing.T) {
	f := Build("login", "", "harisa", "ERROR", 48)
	for _, want := range []string{`"login"`, `"harisa"`, "severity>=ERROR", "timestamp>="} {
		if !strings.Contains(f, want) {
			t.Fatalf("missing %q in %q", want, f)
		}
	}
}

func TestBuild_RawFilterPassthrough(t *testing.T) {
	f := Build(`jsonPayload.event="register"`, "", "", "", 24)
	if !strings.Contains(f, `(jsonPayload.event="register")`) {
		t.Fatalf("raw filter not passed through: %q", f)
	}
}

func TestLooksLikeFilter(t *testing.T) {
	cases := map[string]bool{
		"payment failed":          false,
		"harisa":                  false,
		`severity>=ERROR`:         true,
		`jsonPayload.user="x"`:    true,
		"foo AND bar":             true,
	}
	for in, want := range cases {
		if got := looksLikeFilter(in); got != want {
			t.Errorf("looksLikeFilter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMessageOf_JSONPayload(t *testing.T) {
	e := rawEntry{JSONPayload: json.RawMessage(`{"message":"user registered","level":"info"}`)}
	if got := messageOf(e); got != "user registered" {
		t.Fatalf("messageOf = %q, want 'user registered'", got)
	}
	e2 := rawEntry{TextPayload: "line one\nline two"}
	if got := messageOf(e2); got != "line one line two" {
		t.Fatalf("messageOf textPayload = %q", got)
	}
}

func TestShortTrace(t *testing.T) {
	if got := shortTrace("projects/p/traces/abc123"); got != "abc123" {
		t.Fatalf("shortTrace = %q", got)
	}
}
