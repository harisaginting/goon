// Package telegram — personal.go is the bot surface for goon's
// "soul" file (storage/personal.md). Mirrors the web Memory tab's
// Personal segment but as one-line commands.
package telegram

import (
	"context"
	"strings"

	"github.com/harisaginting/goon/internal/personal"
)

// cmdPersonal handles:
//
//	/personal              show current personal.md
//	/personal set <body>   replace personal.md with <body>
//	/personal reset        restore the default personality
func (b *Bot) cmdPersonal(ctx context.Context, chatID int64, args []string) {
	if len(args) == 0 {
		body := personal.Read()
		if body == "" {
			_ = b.Send(ctx, chatID, "(personal.md is empty — run /personal reset to seed the default)")
			return
		}
		b.SendChunked(ctx, chatID, "🧠 "+personal.Path()+"\n\n"+body)
		return
	}
	switch strings.ToLower(args[0]) {
	case "set", "write":
		body := strings.TrimSpace(strings.Join(args[1:], " "))
		if body == "" {
			_ = b.Send(ctx, chatID, "usage: /personal set <body…>\nor send `/personal reset` to restore the default")
			return
		}
		if err := personal.Write(body); err != nil {
			_ = b.Send(ctx, chatID, "✗ save failed: "+err.Error())
			return
		}
		_ = b.Send(ctx, chatID, "✓ personal.md saved ("+oneLine(body, 80)+")")
	case "reset", "default":
		if err := personal.Write(personal.Default); err != nil {
			_ = b.Send(ctx, chatID, "✗ reset failed: "+err.Error())
			return
		}
		_ = b.Send(ctx, chatID, "✓ personal.md reset to default — try /personal to see it")
	default:
		_ = b.Send(ctx, chatID, "usage:\n  /personal             — show current\n  /personal set <body>  — replace\n  /personal reset       — restore default")
	}
}
