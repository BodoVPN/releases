package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWithTrafficCountersKeepsThePolicyAndTurnsOnTheCounters(t *testing.T) {
	config := `{
		// comments are fine, as in any Xray config
		"outbounds": [{"tag": "proxy", "protocol": "freedom"}],
		"policy": {"levels": {"8": {"connIdle": 120}}, "system": {"statsInboundUplink": false}}
	}`
	out, err := WithTrafficCounters(config)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(out), &root); err != nil {
		t.Fatal(err)
	}
	system := root["policy"].(map[string]any)["system"].(map[string]any)
	if system["statsOutboundUplink"] != true || system["statsOutboundDownlink"] != true || system["statsInboundUplink"] != false {
		t.Fatalf("policy.system = %v", system)
	}
	if !strings.Contains(out, `"connIdle":120`) || root["stats"] == nil {
		t.Fatalf("config = %s", out)
	}
	if _, err := WithTrafficCounters(`{"policy": 3}`); err == nil {
		t.Fatal("a policy that is not an object was accepted")
	}
	if _, err := WithTrafficCounters(`not json`); err == nil {
		t.Fatal("broken JSON was accepted")
	}
}

func TestTheExportedTunnelCountsAfterWithTrafficCounters(t *testing.T) {
	t.Cleanup(func() { _ = StopTunnel() })
	site := newTarget(t, nil)
	config, err := WithTrafficCounters(`{"log":{"loglevel":"none"},"outbounds":[{"tag":"proxy","protocol":"freedom"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunTunnel(config); err != nil {
		t.Fatal(err)
	}
	getThrough(t, tunnel.server, "proxy", site.url())
	counters, err := TunnelCounters()
	if err != nil || counters["outbound"]["proxy"]["downlink"] <= 0 {
		t.Fatalf("counters = %v, %v", counters, err)
	}
}
