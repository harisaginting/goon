package workflow

import "os"

// osReadFile is an indirection so a single helper in hooks_test.go can pull
// file contents back without each test importing os.
var osReadFile = func(p string) ([]byte, error) { return os.ReadFile(p) }
