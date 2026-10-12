package engine

import (
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"time"
)

// The Go runtime's budget inside an iOS Packet Tunnel, which iOS kills at about 50 MiB of
// footprint for the whole process (BodoVPN/bodovpn-client#195). runtime_ios.go applies it in place
// of libXray's iOS policy (GOGC 10, a 30 MiB limit and a forced FreeOSMemory every second), which
// collected back to back near its limit. Untagged so its tests run on every platform. Prior art
// for the numbers: the themoein100/libXray-apple fork.
const (
	// iosMemoryLimit leaves the rest of the ~50 MiB to Swift, NetworkExtension and native code.
	iosMemoryLimit = 36 << 20
	// iosGCPercent lets the heap double between collections; the memory limit is the real
	// ceiling, and a lower value only adds collections the limit would trigger anyway.
	iosGCPercent = 100
	// iosMaxProcs lets the collector run beside packet work without one P per core.
	iosMaxProcs = 2
	// iosTrimEvery and iosTrimAbovePercent: return freed pages only when the in-use heap nears
	// the limit, since FreeOSMemory stops the world.
	iosTrimEvery        = 30 * time.Second
	iosTrimAbovePercent = 90
)

func applyIOSBudget() {
	debug.SetGCPercent(iosGCPercent)
	debug.SetMemoryLimit(iosMemoryLimit)
	if runtime.NumCPU() > iosMaxProcs {
		runtime.GOMAXPROCS(iosMaxProcs)
	}
}

func iosTrimLoop() {
	inUse := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	for {
		time.Sleep(iosTrimEvery)
		// runtime/metrics reads without stopping the world, unlike ReadMemStats.
		metrics.Read(inUse)
		if inUse[0].Value.Kind() == metrics.KindUint64 && shouldTrim(inUse[0].Value.Uint64()) {
			debug.FreeOSMemory()
		}
	}
}

// shouldTrim reports whether an in-use heap of inUse bytes is above iosTrimAbovePercent of the
// iOS memory limit.
func shouldTrim(inUse uint64) bool {
	return inUse > iosMemoryLimit/100*iosTrimAbovePercent
}
