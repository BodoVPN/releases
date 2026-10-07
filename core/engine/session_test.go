package engine

import (
	"strings"
	"testing"
)

func TestRunCountsTrafficInProcessAndStops(t *testing.T) {
	stopTunnel(t)
	site := newTarget(t, nil)
	if response := invokeFor(t, MethodRunXray, map[string]any{"xrayJson": statsConfig(t)}); !response.Success {
		t.Fatalf("runXray: %s", response.Error)
	}
	if state := dataAs[statePayload](t, invokeFor(t, MethodGetXrayState, nil)); !state.Running {
		t.Fatal("getXrayState says no core runs")
	}

	getThrough(t, tunnel.server, "proxy", site.url())

	response := invokeFor(t, MethodQueryStats, nil)
	if !response.Success {
		t.Fatalf("queryStats: %s", response.Error)
	}
	proxy := dataAs[Counters](t, response)["outbound"]["proxy"]
	if proxy["uplink"] <= 0 || proxy["downlink"] <= 0 {
		t.Fatalf("proxy counters = %v, want both directions counted", proxy)
	}

	if response := invokeFor(t, MethodStopXray, nil); !response.Success {
		t.Fatalf("stopXray: %s", response.Error)
	}
	if state := dataAs[statePayload](t, invokeFor(t, MethodGetXrayState, nil)); state.Running {
		t.Fatal("getXrayState says a core still runs")
	}
	if response := invokeFor(t, MethodQueryStats, nil); response.Success || response.Error != ErrNotRunning.Error() {
		t.Fatalf("queryStats after stop = %+v, want %q", response, ErrNotRunning)
	}
}

func TestRunRefusesASecondCore(t *testing.T) {
	stopTunnel(t)
	if err := tunnel.run(statsConfig(t)); err != nil {
		t.Fatal(err)
	}
	response := invokeFor(t, MethodRunXray, map[string]any{"xrayJson": statsConfig(t)})
	if response.Success || response.Error != ErrAlreadyRunning.Error() {
		t.Fatalf("second runXray = %+v, want %q", response, ErrAlreadyRunning)
	}
}

func TestStopWithoutACoreSucceeds(t *testing.T) {
	if response := invokeFor(t, MethodStopXray, nil); !response.Success {
		t.Fatalf("stopXray: %s", response.Error)
	}
}

func TestQueryStatsNamesAConfigWithoutCounters(t *testing.T) {
	stopTunnel(t)
	if err := tunnel.run(freedomConfig(t)); err != nil {
		t.Fatal(err)
	}
	if response := invokeFor(t, MethodQueryStats, nil); response.Success || response.Error != ErrNoCounters.Error() {
		t.Fatalf("queryStats = %+v, want %q", response, ErrNoCounters)
	}
}

func TestRunReportsARefusedConfig(t *testing.T) {
	response := invokeFor(t, MethodRunXray, map[string]any{"xrayJson": `{"outbounds":[{"protocol":"nope"}]}`})
	if response.Success || response.Error == "" {
		t.Fatalf("runXray of a bad config = %+v, want an error", response)
	}
	if tunnel.running() {
		t.Fatal("a refused config left a core running")
	}
}

func TestLibXrayCoreBuildersWaitForTheTunnelCore(t *testing.T) {
	stopTunnel(t)
	if err := tunnel.run(statsConfig(t)); err != nil {
		t.Fatal(err)
	}
	response := invokeFor(t, libXrayTestXray, map[string]any{"xrayJson": freedomConfig(t)})
	if response.Success || !strings.Contains(response.Error, "without a running core") {
		t.Fatalf("testXray beside the tunnel core = %+v, want a refusal", response)
	}
	if err := tunnel.stop(); err != nil {
		t.Fatal(err)
	}
	if response := invokeFor(t, libXrayTestXray, map[string]any{"xrayJson": freedomConfig(t)}); !response.Success {
		t.Fatalf("testXray alone: %s", response.Error)
	}
}
