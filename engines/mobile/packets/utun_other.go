//go:build !darwin

package packets

import "errors"

// FindUtunFD only exists on iOS and macOS.
func FindUtunFD() (int, error) { return -1, errors.New("packets: utun is darwin only") }
