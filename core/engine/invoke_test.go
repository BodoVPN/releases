package engine

import (
	"strings"
	"testing"

	"github.com/xtls/xray-core/core"
)

func TestInvokeHandsLibXrayItsOwnMethods(t *testing.T) {
	response := invokeFor(t, "xrayVersion", nil)
	if !response.Success {
		t.Fatalf("xrayVersion: %s", response.Error)
	}
	if got := dataAs[map[string]string](t, response)["version"]; got != core.Version() {
		t.Fatalf("xrayVersion = %q, want %q", got, core.Version())
	}
	if response := invokeFor(t, "noSuchMethod", nil); response.Success || response.Error != "unknown method" {
		t.Fatalf("unknown method = %+v, want libXray's refusal", response)
	}
}

func TestInvokeRefusesAnotherAPIVersion(t *testing.T) {
	response := decodeResponse(t, Invoke(`{"apiVersion":2,"method":"xrayVersion"}`))
	if response.Success || response.Error != "unsupported apiVersion" {
		t.Fatalf("apiVersion 2 = %+v", response)
	}
}

func TestInvokeRefusesAnUnreadableRequest(t *testing.T) {
	if response := decodeResponse(t, Invoke(`{"apiVersion":`)); response.Success || response.Error == "" {
		t.Fatalf("broken JSON = %+v, want an error", response)
	}
	huge := `{"apiVersion":3,"method":"xrayVersion","payload":"` + strings.Repeat("x", maxInvokeBytes) + `"}`
	if response := decodeResponse(t, Invoke(huge)); response.Success || !strings.Contains(response.Error, "size limit") {
		t.Fatalf("oversized request = %+v, want the size limit", response)
	}
}

func TestBuildInfoNamesTheCore(t *testing.T) {
	response := invokeFor(t, MethodBuildInfo, nil)
	if !response.Success {
		t.Fatalf("buildInfo: %s", response.Error)
	}
	info := dataAs[BuildInfo](t, response)
	if info.Release != "dev" || info.Xray != core.Version() || info.LibXray != "v1.260930.0" || !strings.HasPrefix(info.Go, "go") {
		t.Fatalf("buildInfo = %+v", info)
	}
}
