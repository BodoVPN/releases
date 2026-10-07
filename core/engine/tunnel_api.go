package engine

import (
	"encoding/json"
	"fmt"
)

// RunTunnel starts the process's tunnel core on xrayJSON, as Invoke's runXray does.
func RunTunnel(xrayJSON string) error {
	return tunnel.run(xrayJSON)
}

// StopTunnel stops the tunnel core; a no-op when none runs.
func StopTunnel() error {
	return tunnel.stop()
}

// TunnelCounters are the running tunnel core's traffic counters, as Invoke's queryStats.
func TunnelCounters() (Counters, error) {
	return tunnel.counters()
}

// WithTrafficCounters turns on the outbound traffic counters in xrayJSON: `stats`, and the
// policy's statsOutboundUplink and statsOutboundDownlink, keeping the rest of any policy.
func WithTrafficCounters(xrayJSON string) (string, error) {
	root, err := decodeObject(xrayJSON)
	if err != nil {
		return "", err
	}
	if _, ok := root["stats"]; !ok {
		root["stats"] = map[string]any{}
	}
	policy, ok := root["policy"].(map[string]any)
	if root["policy"] != nil && !ok {
		return "", fmt.Errorf("the config's policy is not an object")
	}
	if policy == nil {
		policy = map[string]any{}
	}
	system, ok := policy["system"].(map[string]any)
	if policy["system"] != nil && !ok {
		return "", fmt.Errorf("the config's policy.system is not an object")
	}
	if system == nil {
		system = map[string]any{}
	}
	system["statsOutboundUplink"] = true
	system["statsOutboundDownlink"] = true
	policy["system"] = system
	root["policy"] = policy
	raw, err := json.Marshal(root)
	return string(raw), err
}
