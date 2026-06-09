package tools

import "context"

// workDirKey carries the per-run working directory through context so
// tools (run_command, search_code) operate in the SELECTED repo instead
// of goon's launch directory. Without this, the agent edited/searched
// goon's own code for every ticket, no matter which repo was chosen.
type workDirKey struct{}

// WithWorkDir returns a context that makes tool execution use dir as the
// working directory. Empty dir is a no-op (tools fall back to the process
// cwd). Set by the workflow execute phase to the ticket's local checkout.
func WithWorkDir(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, workDirKey{}, dir)
}

// WorkDirFrom returns the working directory set by WithWorkDir, or "" when
// none is set (callers then use the process cwd).
func WorkDirFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if d, ok := ctx.Value(workDirKey{}).(string); ok {
		return d
	}
	return ""
}
