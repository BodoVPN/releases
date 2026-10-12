//go:build !ios

package engine

import "github.com/xtls/libxray/memory"

// initRuntime keeps libXray's policy for the platform (Android's own, none on the desktops).
func initRuntime() {
	memory.InitForceFree()
}

func afterStop() {}
