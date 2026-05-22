// Package telegram — personal.go used to expose a /personal command
// that managed a separate storage/personal.md file. That file is
// gone: character and project knowledge now live together in
// SOUL.md (see internal/notes). This shim keeps the command name
// alive so users with muscle memory get a one-line redirect
// instead of a "command not allowed" error.
package telegram

import (
	"context"
)

// cmdPersonal prints a deprecation hint pointing at the SOUL.md
// surface. Kept as a method on *Bot so the existing dispatcher in
// commands.go doesn't need to be threaded around the rename.
func (b *Bot) cmdPersonal(ctx context.Context, chatID int64, args []string) {
	_ = args
	_ = b.Send(ctx, chatID,
		"the /personal command is gone — character and project knowledge\n"+
			"now both live in SOUL.md (one always-loaded file). use:\n\n"+
			"  /knowledge              — show SOUL.md + topic notes\n"+
			"  /memory edit SOUL.md    — edit the file directly\n"+
			"  /memory read SOUL.md    — read the current body\n")
}
