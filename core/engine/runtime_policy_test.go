package engine

import (
	"runtime"
	"runtime/debug"
	"testing"
)

func TestIOSBudget(t *testing.T) {
	previousGC := debug.SetGCPercent(10)
	previousLimit := debug.SetMemoryLimit(30 << 20)
	previousProcs := runtime.GOMAXPROCS(0)
	t.Cleanup(func() {
		debug.SetGCPercent(previousGC)
		debug.SetMemoryLimit(previousLimit)
		runtime.GOMAXPROCS(previousProcs)
	})

	applyIOSBudget()

	if got := debug.SetGCPercent(iosGCPercent); got != iosGCPercent {
		t.Errorf("GOGC = %d, want %d", got, iosGCPercent)
	}
	if got := debug.SetMemoryLimit(-1); got != iosMemoryLimit {
		t.Errorf("memory limit = %d, want %d", got, iosMemoryLimit)
	}
	if got := runtime.GOMAXPROCS(0); got > iosMaxProcs {
		t.Errorf("GOMAXPROCS = %d, want at most %d", got, iosMaxProcs)
	}
}

func TestTrimOnlyNearTheLimit(t *testing.T) {
	threshold := uint64(iosMemoryLimit / 100 * iosTrimAbovePercent)
	for _, tc := range []struct {
		inUse uint64
		want  bool
	}{
		{0, false},
		{iosMemoryLimit / 2, false},
		{threshold, false},
		{threshold + 1, true},
		{iosMemoryLimit, true},
	} {
		if got := shouldTrim(tc.inUse); got != tc.want {
			t.Errorf("shouldTrim(%d) = %v, want %v", tc.inUse, got, tc.want)
		}
	}
}
