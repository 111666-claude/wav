//go:build windows
// +build windows

package wav

import "testing"

// assertSparseAllocation is a no-op on platforms without POSIX block stats.
func assertSparseAllocation(t *testing.T, path string, logicalSize int64) {}
