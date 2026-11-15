//go:build !android

package engine

// setSocketProtector is Android only.
func setSocketProtector(func(fd int) bool) {}
