package cmd

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// Build-time stamps. Set via -ldflags at link time:
//
//	go build -ldflags "-X github.com/harisaginting/goon/cmd.version=v0.2.0 \
//	  -X github.com/harisaginting/goon/cmd.commit=$(git rev-parse --short HEAD) \
//	  -X github.com/harisaginting/goon/cmd.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// When unstamped (e.g. `go run .` from a checkout), we fall back to the
// debug.BuildInfo VCS data Go embeds automatically since 1.18, so users
// reporting a bug always have *something* useful in `goon --version`.
var (
	version = ""
	commit  = ""
	date    = ""
)

// printVersion writes a one-line version string covering version, short
// commit, build date, and Go runtime info. Designed so a user pasting
// `goon --version` into a bug report gives the maintainer enough info to
// reproduce.
func printVersion(w io.Writer) {
	v, c, d := resolveBuildStamps()
	fmt.Fprintf(w, "goon %s (commit %s, built %s, %s)\n",
		v, c, d, runtime.Version())
}

// resolveBuildStamps returns (version, commit, date) — preferring ldflags
// values when set, falling back to debug.ReadBuildInfo for `go install`-ed
// or `go run .`-from-checkout binaries.
func resolveBuildStamps() (string, string, string) {
	v, c, d := version, commit, date
	if v == "" {
		v = "dev"
	}
	if c == "" || d == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if c == "" && s.Value != "" {
						c = s.Value
						if len(c) > 12 {
							c = c[:12]
						}
					}
				case "vcs.time":
					if d == "" {
						d = s.Value
					}
				}
			}
			if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
				v = info.Main.Version
			}
		}
	}
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	return v, c, d
}

// Version returns the resolved version string for programmatic callers
// (e.g. the web UI's About panel and the Telegram bot's /whoami).
func Version() string {
	v, _, _ := resolveBuildStamps()
	return v
}

// FullVersion returns "version (commit X, built Y)" for status banners.
func FullVersion() string {
	v, c, d := resolveBuildStamps()
	return fmt.Sprintf("%s (commit %s, built %s)", v, c, d)
}
