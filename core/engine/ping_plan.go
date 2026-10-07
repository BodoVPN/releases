package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/xtls/xray-core/infra/conf"
	confjson "github.com/xtls/xray-core/infra/conf/json"
)

// pingTagPrefix names the probed outbounds in the merged core; a config's own outbounds
// with this prefix are dropped, so a probe can never reach the wrong server.
const pingTagPrefix = "bodo-ping-"

// pingItem is one config, ready to merge: its probed outbound and the outbounds it dials
// through, renamed into its own namespace, plus the sections the core may need beside them.
type pingItem struct {
	index     int
	tag       string
	outbounds []map[string]any
	hosts     []string
	support   []map[string]any
	dns       map[string]any
	routing   map[string]any
	policy    map[string]any
}

func planPingItem(index int, config PingConfig) (pingItem, error) {
	root, err := decodeObject(config.XrayJSON)
	if err != nil {
		return pingItem{}, err
	}
	outbounds, err := objectList(root["outbounds"], "outbounds")
	if err != nil {
		return pingItem{}, err
	}
	if len(outbounds) == 0 {
		return pingItem{}, errors.New("the config has no outbounds")
	}
	tags, duplicates := indexTags(outbounds)
	target, err := targetIndex(outbounds, tags, duplicates, config.OutboundTag)
	if err != nil {
		return pingItem{}, err
	}
	chain, err := dialChain(outbounds, tags, duplicates, target)
	if err != nil {
		return pingItem{}, err
	}
	names := make(map[int]string, len(chain))
	for order, at := range chain {
		names[at] = fmt.Sprintf("%s%d-%d", pingTagPrefix, index, order)
	}
	item := pingItem{index: index, tag: names[target]}
	for _, at := range chain {
		outbound := deepCopyObject(outbounds[at])
		outbound["tag"] = names[at]
		for _, ref := range outboundRefs(outbound) {
			ref.set(names[tags[ref.tag]])
		}
		if err := validateOutbound(outbound); err != nil {
			return pingItem{}, fmt.Errorf("outbound %q: %w", tagOf(outbounds[at]), err)
		}
		item.outbounds = append(item.outbounds, outbound)
		item.hosts = append(item.hosts, serverDomains(outbound)...)
	}
	for at, outbound := range outbounds {
		tag := tagOf(outbound)
		if _, probed := names[at]; probed || tag == "" || strings.HasPrefix(tag, pingTagPrefix) {
			continue
		}
		item.support = append(item.support, deepCopyObject(outbound))
	}
	item.dns, _ = root["dns"].(map[string]any)
	item.routing, _ = root["routing"].(map[string]any)
	item.policy, _ = root["policy"].(map[string]any)
	return item, nil
}

func decodeObject(text string) (map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("the config is empty")
	}
	decoder := json.NewDecoder(&confjson.Reader{Reader: strings.NewReader(text)})
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("the config is not a JSON object: %w", err)
	}
	if root == nil {
		return nil, errors.New("the config is not a JSON object")
	}
	if decoder.More() {
		return nil, errors.New("the config holds more than one JSON value")
	}
	return root, nil
}

func objectList(value any, name string) ([]map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a list", name)
	}
	objects := make([]map[string]any, 0, len(list))
	for i, entry := range list {
		object, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] is not an object", name, i)
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func tagOf(outbound map[string]any) string {
	tag, _ := outbound["tag"].(string)
	return tag
}

func indexTags(outbounds []map[string]any) (map[string]int, map[string]bool) {
	tags := map[string]int{}
	duplicates := map[string]bool{}
	for at, outbound := range outbounds {
		tag := tagOf(outbound)
		if tag == "" {
			continue
		}
		if _, seen := tags[tag]; seen {
			duplicates[tag] = true
			continue
		}
		tags[tag] = at
	}
	return tags, duplicates
}

func targetIndex(outbounds []map[string]any, tags map[string]int, duplicates map[string]bool, requested string) (int, error) {
	tag := requested
	if tag == "" {
		if _, ok := tags["proxy"]; !ok {
			return 0, nil
		}
		tag = "proxy"
	}
	if duplicates[tag] {
		return 0, fmt.Errorf("more than one outbound is tagged %q", tag)
	}
	at, ok := tags[tag]
	if !ok {
		return 0, fmt.Errorf("no outbound is tagged %q", tag)
	}
	return at, nil
}

// dialChain is the target followed by every outbound it dials through, depth first, each once.
func dialChain(outbounds []map[string]any, tags map[string]int, duplicates map[string]bool, target int) ([]int, error) {
	const (
		visiting = 1
		visited  = 2
	)
	state := make([]int, len(outbounds))
	var chain []int
	var visit func(at int) error
	visit = func(at int) error {
		switch state[at] {
		case visiting:
			return fmt.Errorf("outbound %q dials through itself", tagOf(outbounds[at]))
		case visited:
			return nil
		}
		state[at] = visiting
		chain = append(chain, at)
		for _, ref := range outboundRefs(outbounds[at]) {
			if duplicates[ref.tag] {
				return fmt.Errorf("outbound %q dials through %q, which more than one outbound is tagged", tagOf(outbounds[at]), ref.tag)
			}
			next, ok := tags[ref.tag]
			if !ok {
				return fmt.Errorf("outbound %q dials through %q, which no outbound is tagged", tagOf(outbounds[at]), ref.tag)
			}
			if err := visit(next); err != nil {
				return err
			}
		}
		state[at] = visited
		return nil
	}
	if err := visit(target); err != nil {
		return nil, err
	}
	return chain, nil
}

type outboundRef struct {
	tag string
	set func(string)
}

// outboundRefs are the outbounds this one dials through. Xray dropped proxySettings for
// sockopt.dialerProxy, and refuses a config that still has it.
func outboundRefs(outbound map[string]any) []outboundRef {
	stream, _ := outbound["streamSettings"].(map[string]any)
	sockopt, _ := stream["sockopt"].(map[string]any)
	if tag, ok := sockopt["dialerProxy"].(string); ok && tag != "" {
		return []outboundRef{{tag, func(name string) { sockopt["dialerProxy"] = name }}}
	}
	return nil
}

// serverDomains are the domain names an outbound dials (its servers, peers and xhttp's
// download server), lower case; IP literals need no lookup.
func serverDomains(outbound map[string]any) []string {
	var addresses []string
	settings, _ := outbound["settings"].(map[string]any)
	if address, ok := settings["address"].(string); ok {
		addresses = append(addresses, address)
	}
	for _, key := range []string{"vnext", "servers"} {
		list, _ := settings[key].([]any)
		for _, entry := range list {
			if server, ok := entry.(map[string]any); ok {
				if address, ok := server["address"].(string); ok {
					addresses = append(addresses, address)
				}
			}
		}
	}
	peers, _ := settings["peers"].([]any)
	for _, entry := range peers {
		if peer, ok := entry.(map[string]any); ok {
			if endpoint, ok := peer["endpoint"].(string); ok {
				host, _, err := net.SplitHostPort(endpoint)
				if err != nil {
					host = endpoint
				}
				addresses = append(addresses, host)
			}
		}
	}
	stream, _ := outbound["streamSettings"].(map[string]any)
	for _, key := range []string{"xhttpSettings", "splithttpSettings"} {
		xhttp, _ := stream[key].(map[string]any)
		extra, _ := xhttp["extra"].(map[string]any)
		for _, holder := range []map[string]any{xhttp, extra} {
			download, _ := holder["downloadSettings"].(map[string]any)
			if address, ok := download["address"].(string); ok {
				addresses = append(addresses, address)
			}
		}
	}
	var domains []string
	for _, address := range addresses {
		if address != "" && net.ParseIP(strings.Trim(address, "[]")) == nil {
			domains = append(domains, strings.ToLower(address))
		}
	}
	return domains
}

func validateOutbound(outbound map[string]any) error {
	raw, err := json.Marshal(outbound)
	if err != nil {
		return err
	}
	var detour conf.OutboundDetourConfig
	if err := json.Unmarshal(raw, &detour); err != nil {
		return err
	}
	_, err = detour.Build()
	return err
}

func deepCopyObject(object map[string]any) map[string]any {
	copied := make(map[string]any, len(object))
	for key, value := range object {
		copied[key] = deepCopy(value)
	}
	return copied
}

func deepCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return deepCopyObject(typed)
	case []any:
		copied := make([]any, len(typed))
		for i, entry := range typed {
			copied[i] = deepCopy(entry)
		}
		return copied
	default:
		return value
	}
}
