// Package personal is DEPRECATED — kept as a build-clean stub.
//
// Character / voice content used to live in a separate
// ./storage/personal.md, managed through this package. As of this
// version it's folded into ./storage/memory/SOUL.md so users have
// ONE always-loaded context file instead of two. See:
//
//   - internal/notes.SeedSoulTemplate    seeds the unified template
//   - internal/notes.MergePersonalIntoSoul  one-shot migration that
//     prepends an existing personal.md into SOUL.md and renames the
//     original to personal.md.bak so the user can verify the merge.
//
// Nothing in the tree imports this package anymore — the file is
// kept only so the directory doesn't 404 against external scripts
// or tags. Safe to remove: `rm -rf internal/personal`.
package personal
