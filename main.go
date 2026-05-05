// goon
//
// Usage:
//
//	goon "list .log files older than 30 days"          # dry-run
//	goon "delete the build directory" --run            # ask before each step
//	goon "tidy go.mod" --auto                          # validated, no prompt
//	goon "explain how a Makefile works" --explain      # planning only
//
// See README.md for full documentation.
package main

import (
	"fmt"
	"os"

	"github.com/harisaginting/goon/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "goon: "+err.Error())
		os.Exit(1)
	}
}
