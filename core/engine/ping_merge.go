package engine

import (
	"bytes"
	"encoding/json"
	"fmt"

	applog "github.com/xtls/xray-core/app/log"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	confserial "github.com/xtls/xray-core/infra/conf/serial"
)

func buildPingCoreConfig(items []pingItem) (*core.Config, error) {
	raw, err := json.Marshal(mergePingItems(items))
	if err != nil {
		return nil, err
	}
	jsonConfig, err := confserial.DecodeJSONConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	config, err := jsonConfig.Build()
	if err != nil {
		return nil, err
	}
	config.App = withoutLogApp(config.App)
	return config, nil
}

// mergePingItems is one core for the items: every probed outbound first, then the other
// tagged outbounds (first config wins a tag), the routing rules that reach those, the DNS
// servers with their domains joined, and the first policy. No inbounds, log, stats or env.
func mergePingItems(items []pingItem) map[string]any {
	var outbounds []any
	for _, item := range items {
		for _, outbound := range item.outbounds {
			outbounds = append(outbounds, outbound)
		}
	}
	support := map[string]bool{}
	for _, item := range items {
		for _, outbound := range item.support {
			if tag := tagOf(outbound); !support[tag] {
				support[tag] = true
				outbounds = append(outbounds, outbound)
			}
		}
	}
	merged := map[string]any{"outbounds": outbounds}
	if routing := mergeRouting(items, support); routing != nil {
		merged["routing"] = routing
	}
	if dns := mergeDNS(items); dns != nil {
		merged["dns"] = dns
	}
	for _, item := range items {
		if item.policy != nil {
			merged["policy"] = item.policy
			break
		}
	}
	return merged
}

// withoutLogApp drops the log app Build always adds: starting it would make it the process's
// log handler, and closing it would silence a running tunnel core's log.
func withoutLogApp(apps []*serial.TypedMessage) []*serial.TypedMessage {
	logType := serial.GetMessageType(&applog.Config{})
	kept := apps[:0]
	for _, app := range apps {
		if app.Type != logType {
			kept = append(kept, app)
		}
	}
	return kept
}

func mergeRouting(items []pingItem, support map[string]bool) map[string]any {
	var routing map[string]any
	var rules []any
	seen := map[string]bool{}
	for _, item := range items {
		if item.routing == nil {
			continue
		}
		if routing == nil {
			routing = map[string]any{}
			for key, value := range item.routing {
				if key != "rules" && key != "balancers" {
					routing[key] = value
				}
			}
		}
		list, _ := item.routing["rules"].([]any)
		for _, entry := range list {
			rule, ok := entry.(map[string]any)
			if !ok || rule["balancerTag"] != nil {
				continue
			}
			if tag, _ := rule["outboundTag"].(string); !support[tag] {
				continue
			}
			if key := canonical(rule); !seen[key] {
				seen[key] = true
				rules = append(rules, rule)
			}
		}
	}
	if routing != nil && len(rules) > 0 {
		routing["rules"] = rules
	}
	return routing
}

func mergeDNS(items []pingItem) map[string]any {
	var dns map[string]any
	var servers []any
	serverAt := map[string]int{}
	hosts := map[string]any{}
	for _, item := range items {
		if item.dns == nil {
			continue
		}
		if dns == nil {
			dns = map[string]any{}
			for key, value := range item.dns {
				if key != "servers" && key != "hosts" {
					dns[key] = value
				}
			}
		}
		if itemHosts, ok := item.dns["hosts"].(map[string]any); ok {
			for name, address := range itemHosts {
				if _, taken := hosts[name]; !taken {
					hosts[name] = address
				}
			}
		}
		list, _ := item.dns["servers"].([]any)
		for _, entry := range list {
			servers = mergeServer(servers, serverAt, entry)
		}
	}
	if dns == nil {
		return nil
	}
	if len(servers) > 0 {
		dns["servers"] = servers
	}
	if len(hosts) > 0 {
		dns["hosts"] = hosts
	}
	return dns
}

// mergeServer adds a DNS server; one equal to a server already there except for its domains
// joins its domains to that one, so every config's names still resolve where they did.
func mergeServer(servers []any, serverAt map[string]int, entry any) []any {
	server, isObject := entry.(map[string]any)
	if !isObject {
		key := canonical(entry)
		if _, seen := serverAt[key]; !seen {
			serverAt[key] = len(servers)
			servers = append(servers, entry)
		}
		return servers
	}
	rest := deepCopyObject(server)
	delete(rest, "domains")
	key := canonical(rest)
	at, seen := serverAt[key]
	if !seen {
		serverAt[key] = len(servers)
		return append(servers, deepCopyObject(server))
	}
	existing := servers[at].(map[string]any)
	domains, _ := existing["domains"].([]any)
	added, _ := server["domains"].([]any)
	known := map[string]bool{}
	for _, domain := range domains {
		known[canonical(domain)] = true
	}
	for _, domain := range added {
		if key := canonical(domain); !known[key] {
			known[key] = true
			domains = append(domains, domain)
		}
	}
	if domains != nil {
		existing["domains"] = domains
	}
	return servers
}

func canonical(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%#v", value)
	}
	return string(raw)
}
