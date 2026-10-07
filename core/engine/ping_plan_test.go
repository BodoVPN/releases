package engine

import (
	"reflect"
	"strings"
	"testing"

	applog "github.com/xtls/xray-core/app/log"
	"github.com/xtls/xray-core/common/serial"
)

// decoratedConfig has the shape the app's decorator gives a desktop probe: a domain exit
// resolved over bootstrap DoH scoped to that host, which egresses direct.
func decoratedConfig(t *testing.T, host string) string {
	t.Helper()
	return mustJSON(t, map[string]any{
		"log":      map[string]any{"loglevel": "warning"},
		"env":      map[string]any{"xray.location.asset": "/nowhere"},
		"inbounds": []any{map[string]any{"tag": "in_proxy", "port": 10807, "protocol": "socks"}},
		"outbounds": []any{
			map[string]any{
				"tag": "proxy", "protocol": "vless",
				"settings": map[string]any{"vnext": []any{map[string]any{
					"address": host, "port": 443,
					"users": []any{map[string]any{"id": "27848739-7e62-4138-9fd3-098a63964b6b", "encryption": "none"}},
				}}},
				"streamSettings": map[string]any{"sockopt": map[string]any{"domainStrategy": "UseIPv4"}},
			},
			map[string]any{"tag": "direct", "protocol": "freedom"},
			map[string]any{"tag": "blackhole", "protocol": "blackhole"},
		},
		"dns": map[string]any{
			"queryStrategy": "UseIPv4",
			"servers": []any{
				map[string]any{"address": "https://1.1.1.1/dns-query", "domains": []any{"full:" + host}, "skipFallback": true},
				map[string]any{"address": "https://8.8.8.8/dns-query", "domains": []any{"full:" + host}, "skipFallback": true},
			},
		},
		"routing": map[string]any{
			"domainStrategy": "AsIs",
			"rules": []any{
				map[string]any{"ip": []any{"1.1.1.1", "8.8.8.8"}, "port": "443", "network": "tcp", "outboundTag": "direct"},
				map[string]any{"domain": []any{"geosite:category-ads"}, "outboundTag": "proxy"},
				map[string]any{"network": "udp", "balancerTag": "spread"},
			},
		},
		"policy": map[string]any{"levels": map[string]any{"0": map[string]any{"handshake": 4}}},
		"stats":  map[string]any{},
	})
}

func planAll(t *testing.T, configs ...string) []pingItem {
	t.Helper()
	var items []pingItem
	for i, config := range configs {
		item, err := planPingItem(i, PingConfig{XrayJSON: config})
		if err != nil {
			t.Fatalf("config %d: %v", i, err)
		}
		items = append(items, item)
	}
	return items
}

func tagsIn(t *testing.T, merged map[string]any) []string {
	t.Helper()
	var tags []string
	for _, outbound := range merged["outbounds"].([]any) {
		tags = append(tags, tagOf(outbound.(map[string]any)))
	}
	return tags
}

func TestMergeKeepsWhatTheProbesNeedAndNothingElse(t *testing.T) {
	merged := mergePingItems(planAll(t, decoratedConfig(t, "a.example"), decoratedConfig(t, "b.example")))

	want := []string{"bodo-ping-0-0", "bodo-ping-1-0", "direct", "blackhole"}
	if got := tagsIn(t, merged); !reflect.DeepEqual(got, want) {
		t.Fatalf("outbounds = %v, want %v", got, want)
	}
	for _, key := range []string{"log", "env", "inbounds", "stats"} {
		if _, ok := merged[key]; ok {
			t.Fatalf("the merged core keeps %q", key)
		}
	}
	servers := merged["dns"].(map[string]any)["servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("dns servers = %v, want the two DoH servers once each", servers)
	}
	for _, server := range servers {
		domains := server.(map[string]any)["domains"].([]any)
		if !reflect.DeepEqual(domains, []any{"full:a.example", "full:b.example"}) {
			t.Fatalf("domains = %v, want both exit hosts", domains)
		}
	}
	rules := merged["routing"].(map[string]any)["rules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["outboundTag"] != "direct" {
		t.Fatalf("rules = %v, want only the one DoH rule to direct", rules)
	}
	if merged["policy"] == nil {
		t.Fatal("the merged core lost the policy")
	}
}

func TestMergedCoreBuildsWithoutALogApp(t *testing.T) {
	config, err := buildPingCoreConfig(planAll(t, decoratedConfig(t, "a.example"), decoratedConfig(t, "b.example")))
	if err != nil {
		t.Fatal(err)
	}
	logType := serial.GetMessageType(&applog.Config{})
	for _, app := range config.App {
		if app.Type == logType {
			t.Fatal("the merged core would start a log app")
		}
	}
}

func TestPlanRenamesTheDialChain(t *testing.T) {
	config := mustJSON(t, map[string]any{"outbounds": []any{
		map[string]any{"tag": "direct", "protocol": "freedom"},
		map[string]any{
			"tag": "proxy", "protocol": "freedom",
			"streamSettings": map[string]any{"sockopt": map[string]any{"dialerProxy": "hop"}},
		},
		map[string]any{
			"tag": "hop", "protocol": "freedom",
			"streamSettings": map[string]any{"sockopt": map[string]any{"dialerProxy": "direct"}},
		},
	}})
	item, err := planPingItem(3, PingConfig{XrayJSON: config})
	if err != nil {
		t.Fatal(err)
	}
	if item.tag != "bodo-ping-3-0" || len(item.outbounds) != 3 {
		t.Fatalf("item = %+v", item)
	}
	dialerOf := func(outbound map[string]any) any {
		return outbound["streamSettings"].(map[string]any)["sockopt"].(map[string]any)["dialerProxy"]
	}
	proxy, hop, direct := item.outbounds[0], item.outbounds[1], item.outbounds[2]
	if dialerOf(proxy) != "bodo-ping-3-1" || dialerOf(hop) != "bodo-ping-3-2" || direct["tag"] != "bodo-ping-3-2" {
		t.Fatalf("chain not renamed: %v", item.outbounds)
	}
	site := newTarget(t, nil)
	if results := pingOne(t, PingRequest{Configs: []PingConfig{{XrayJSON: config}}, URL: site.url()}); !results[0].Success {
		t.Fatalf("ping through the chain: %+v", results[0])
	}
	if len(item.support) != 0 {
		t.Fatalf("support = %v, want the chain's own outbounds left out", item.support)
	}
}

func TestPlanRefusesWhatCannotBeProbed(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"two values":     `{"outbounds":[{"protocol":"freedom"}]} {}`,
		"no outbounds":   `{"outbounds":[]}`,
		"not a list":     `{"outbounds":{}}`,
		"not an object":  `{"outbounds":[1]}`,
		"duplicate tag":  `{"outbounds":[{"tag":"proxy","protocol":"freedom"},{"tag":"proxy","protocol":"freedom"}]}`,
		"missing dialer": `{"outbounds":[{"tag":"proxy","protocol":"freedom","streamSettings":{"sockopt":{"dialerProxy":"gone"}}}]}`,
		"dialer loop":    `{"outbounds":[{"tag":"proxy","protocol":"freedom","streamSettings":{"sockopt":{"dialerProxy":"proxy"}}}]}`,
		"proxySettings":  `{"outbounds":[{"tag":"proxy","protocol":"freedom","proxySettings":{"tag":"direct"}},{"tag":"direct","protocol":"freedom"}]}`,
	}
	for name, config := range cases {
		if _, err := planPingItem(0, PingConfig{XrayJSON: config}); err == nil {
			t.Errorf("%s: planned", name)
		}
	}
}

func TestPlanDefaultsToProxyThenTheFirstOutbound(t *testing.T) {
	first, err := planPingItem(0, PingConfig{XrayJSON: `{"outbounds":[{"tag":"a","protocol":"freedom"},{"tag":"b","protocol":"blackhole"}]}`})
	if err != nil || first.outbounds[0]["protocol"] != "freedom" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	proxy, err := planPingItem(0, PingConfig{XrayJSON: `{"outbounds":[{"tag":"a","protocol":"freedom"},{"tag":"proxy","protocol":"blackhole"}]}`})
	if err != nil || proxy.outbounds[0]["protocol"] != "blackhole" {
		t.Fatalf("proxy = %+v, %v", proxy, err)
	}
	if _, err := planPingItem(0, PingConfig{XrayJSON: `{"outbounds":[{"tag":"bodo-ping-9-9","protocol":"freedom"},{"tag":"proxy","protocol":"freedom"}]}`}); err != nil {
		t.Fatal(err)
	}
}

func TestPlanDropsOutboundsInThePingNamespace(t *testing.T) {
	item, err := planPingItem(0, PingConfig{XrayJSON: `{"outbounds":[{"tag":"proxy","protocol":"freedom"},{"tag":"bodo-ping-0-0","protocol":"blackhole"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	for _, outbound := range item.support {
		if strings.HasPrefix(tagOf(outbound), pingTagPrefix) {
			t.Fatalf("support keeps %v", outbound)
		}
	}
}
