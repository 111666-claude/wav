//go:build !windows
// +build !windows

package wav

import (
	"syscall"
	"testing"
)

// assertSparseAllocation verifies the file backing the 4 GiB test does not
// physically store its sparse hole.
func assertSparseAllocation(t *testing.T, path string, logicalSize int64) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	allocated := st.Blocks * 512
	if allocated >= logicalSize/100 {
		t.Fatalf("expected a sparse file, but %d of %d logical bytes are allocated", allocated, logicalSize)
	}
	t.Logf("sparse file: %d logical bytes, %d bytes allocated on disk", logicalSize, allocated)
}
