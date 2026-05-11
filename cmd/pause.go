package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/harisaginting/goon/internal/memory"
)

// runPause flips the daemon's Paused flag in memory.json. The running
// daemon picks this up on the next poll tick (via memory.Reload) and
// skips pollAndRun until /resume.
//
//	goon pause              — stop polling for new tickets
//	goon resume             — pick up where we left off
//
// Pause is a runtime control, not a config flag — it's cleared on a
// fresh `goon start` so every session starts active.
func runPause(_ context.Context, _ []string, stdout, stderr io.Writer) error {
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if mem.IsPaused() {
		fmt.Fprintln(stdout, "daemon is already paused.")
		return nil
	}
	mem.SetPaused(true)
	fmt.Fprintln(stdout, "✓ daemon paused — running workflows finish, no new tickets are picked up.")
	fmt.Fprintln(stdout, "  Resume with: goon resume")
	if !mem.GetStatus().Running {
		fmt.Fprintln(stderr, "  (note: no daemon is running — pause will apply when you next `goon start`)")
	}
	return nil
}

// runResume clears the Paused flag.
func runResume(_ context.Context, _ []string, stdout, stderr io.Writer) error {
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if !mem.IsPaused() {
		fmt.Fprintln(stdout, "daemon is not paused.")
		return nil
	}
	mem.SetPaused(false)
	fmt.Fprintln(stdout, "✓ daemon resumed — next poll picks up new tickets.")
	if !mem.GetStatus().Running {
		fmt.Fprintln(stderr, "  (note: no daemon is running — start with `goon start`)")
	}
	return nil
}
