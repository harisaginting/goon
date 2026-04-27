package web

import "os"

// getenv is a thin wrapper around os.Getenv split out so tests can stub it.
func getenv(k string) string { return os.Getenv(k) }
