package engine

import (
	"runtime"
	"runtime/debug"

	"github.com/xtls/xray-core/core"
)

const libXrayModule = "github.com/xtls/libxray"

// release is the BodoVPN/releases tag this build was published as, set with -ldflags -X.
var release = "dev"

// BuildInfo names what a core binary was built from, for the apps' logs.
type BuildInfo struct {
	Release string `json:"release"`
	Xray    string `json:"xray"`
	LibXray string `json:"libXray"`
	Go      string `json:"go"`
}

// ReadBuildInfo reports the release, Xray-core's version, libXray's module version and Go's.
func ReadBuildInfo() BuildInfo {
	info := BuildInfo{Release: release, Xray: core.Version(), Go: runtime.Version()}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range build.Deps {
			if dep.Path == libXrayModule {
				info.LibXray = dep.Version
			}
		}
	}
	return info
}
