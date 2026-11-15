//go:build ios

package engine

import "runtime/debug"

// The packet tunnel extension is killed past 50 MB. Keep the Go heap well
// under that: a soft limit makes the GC work harder as it nears, and a lower
// GC percent keeps the heap from doubling between collections.
func init() {
	debug.SetMemoryLimit(memoryLimit)
	debug.SetGCPercent(gcPercent)
}
