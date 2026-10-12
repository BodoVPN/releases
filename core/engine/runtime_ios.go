//go:build ios

package engine

import (
	"runtime/debug"
	"sync"
)

var startTrim sync.Once

// initRuntime applies the iOS budget (runtime_policy.go); it runs on every core start, after
// anything libXray set.
func initRuntime() {
	applyIOSBudget()
	startTrim.Do(func() { go iosTrimLoop() })
}

// afterStop hands the stopped core's heap back to iOS rather than keeping it mapped.
func afterStop() {
	debug.FreeOSMemory()
}
