package web

import "github.com/harisaginting/goon/internal/envstore"

// configFilePath / setConfigKey / unsetConfigKey are thin shims over
// internal/envstore. The shared package was extracted so the Telegram
// bot can use the same writer without web<->telegram import cycles.
func configFilePath() string             { return envstore.Path() }
func setConfigKey(k, v string) error     { return envstore.Set(k, v) }
func unsetConfigKey(k string) error      { return envstore.Unset(k) }
