//go:build tools

// Package engine pins golang.org/x/mobile, which gomobile bind needs in the
// module (see ../scripts/build-engine.sh).
package engine

import _ "golang.org/x/mobile/bind"
