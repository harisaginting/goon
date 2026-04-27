//go:build windows

package memory

// lockFile is a no-op on Windows; goon's daemon mode is Linux/macOS only.
func lockFile(_ string) (func(), error) {
	return func() {}, nil
}
