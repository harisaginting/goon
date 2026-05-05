package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/harisaginting/goon/internal/checkup"
)

// runDoctor probes every configured provider and prints a one-line report
// per component. Exits non-zero if any non-skipped check failed.
//
//	goon doctor              # human-readable
//	goon doctor --json       # machine-readable
//	goon doctor --quiet      # only print failures
func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON instead of pretty text")
	quiet := fs.Bool("quiet", false, "only print failures")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rs := checkup.Run(ctx)

	if *asJSON {
		emitJSON(stdout, rs)
		if !checkup.AllOK(rs) {
			return fmt.Errorf("doctor: %s", checkup.FirstFailure(rs))
		}
		return nil
	}

	for _, r := range rs {
		if *quiet && (r.OK || r.Skipped) {
			continue
		}
		fmt.Fprintln(stdout, r.Pretty())
	}
	if checkup.AllOK(rs) {
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "all checks passed ✓")
		return nil
	}
	return fmt.Errorf("doctor: %s", checkup.FirstFailure(rs))
}

// emitJSON writes the results as a JSON array (no extra deps).
func emitJSON(w io.Writer, rs []checkup.Result) {
	fmt.Fprint(w, "[\n")
	for i, r := range rs {
		comma := ","
		if i == len(rs)-1 {
			comma = ""
		}
		fmt.Fprintf(w,
			`  {"component":%q,"name":%q,"ok":%v,"skipped":%v,"detail":%q}%s`+"\n",
			r.Component, r.Name, r.OK, r.Skipped, r.Detail, comma)
	}
	fmt.Fprint(w, "]\n")
}
