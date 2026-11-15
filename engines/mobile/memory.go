package engine

// Memory settings applied on iOS (memory_ios.go). Exposed for the report and
// for tfdev's -ioslimits flag, which applies the same numbers on a computer.
const (
	memoryLimit = 32 << 20 // Go soft memory limit: 32 MiB
	gcPercent   = 50
)

// IOSMemorySettings returns the soft memory limit and GC percent used on iOS.
func IOSMemorySettings() (limit int64, gcPct int) { return memoryLimit, gcPercent }
