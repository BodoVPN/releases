package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// syncWithCoreStarts orders the caller after every dialer-globals write so far, for the race
// detector, which can't see that a request reached the handler over loopback TCP.
func syncWithCoreStarts() {
	liveCores.Lock()
	liveCores.Unlock()
}

// outcome carries a check's result from the site's handler to the test.
type outcome struct {
	mu  sync.Mutex
	ran bool
	err error
}

func (o *outcome) set(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ran, o.err = true, err
}

func (o *outcome) get() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.ran {
		return errors.New("the check never ran")
	}
	return o.err
}

// siteCheckingDuringTheBatch runs check from its handler on the timed request, while the
// batch's core is alive.
func siteCheckingDuringTheBatch(t *testing.T, check func() error) (*target, *outcome) {
	t.Helper()
	result := &outcome{}
	site := newTarget(t, func(s *target) {
		s.onRequest = func(number int64) {
			if number == 2 {
				syncWithCoreStarts()
				result.set(check())
			}
		}
	})
	return site, result
}

func TestTunnelKeepsItsDialerChainBesideABatch(t *testing.T) {
	stopTunnel(t)
	other := newTarget(t, nil)
	config := mustJSON(t, map[string]any{
		"log": map[string]any{"loglevel": "none"},
		"outbounds": []any{
			map[string]any{
				"tag": "proxy", "protocol": "freedom",
				"streamSettings": map[string]any{"sockopt": map[string]any{"dialerProxy": "hop"}},
			},
			map[string]any{"tag": "hop", "protocol": "freedom"},
		},
	})
	if err := tunnel.run(config); err != nil {
		t.Fatal(err)
	}
	site, during := siteCheckingDuringTheBatch(t, func() error { return fetchThrough(tunnel.server, "proxy", other.url()) })

	results := pingOne(t, PingRequest{Configs: []PingConfig{{XrayJSON: freedomConfig(t)}}, URL: site.url()})
	if !results[0].Success {
		t.Fatalf("ping: %+v", results[0])
	}
	if err := during.get(); err != nil {
		t.Fatalf("the tunnel's dialer chain broke during the batch: %v", err)
	}
	if err := fetchThrough(tunnel.server, "proxy", other.url()); err != nil {
		t.Fatalf("the tunnel's dialer chain broke after the batch: %v", err)
	}
}

func TestEachCoreResolvesItsOwnServersWithItsOwnDNS(t *testing.T) {
	stopTunnel(t)
	const id = "27848739-7e62-4138-9fd3-098a63964b6b"
	port := startServerCore(t, id)
	other := newTarget(t, nil)
	byOwnDNS := map[string]any{"sockopt": map[string]any{"domainStrategy": "UseIPv4"}}
	tunnelConfig := mustJSON(t, map[string]any{
		"log":       map[string]any{"loglevel": "none"},
		"dns":       map[string]any{"hosts": map[string]any{"tunnel.test": "127.0.0.1"}},
		"outbounds": []any{map[string]any{"tag": "proxy", "protocol": "freedom", "streamSettings": byOwnDNS}},
	})
	if err := tunnel.run(tunnelConfig); err != nil {
		t.Fatal(err)
	}
	tunnelURL := strings.Replace(other.url(), "127.0.0.1", "tunnel.test", 1)
	site, during := siteCheckingDuringTheBatch(t, func() error { return fetchThrough(tunnel.server, "proxy", tunnelURL) })
	batchConfig := mustJSON(t, map[string]any{
		"dns": map[string]any{"hosts": map[string]any{"batch.test": "127.0.0.1"}},
		"outbounds": []any{map[string]any{
			"tag": "proxy", "protocol": "vless", "streamSettings": byOwnDNS,
			"settings": map[string]any{"vnext": []any{map[string]any{
				"address": "batch.test", "port": port,
				"users": []any{map[string]any{"id": id, "encryption": "none"}},
			}}},
		}},
	})

	results := pingOne(t, PingRequest{Configs: []PingConfig{{XrayJSON: batchConfig}}, URL: site.url()})
	if !results[0].Success {
		t.Fatalf("the batch's server did not resolve with the batch's DNS: %+v", results[0])
	}
	if err := during.get(); err != nil {
		t.Fatalf("the tunnel's server did not resolve with the tunnel's DNS during the batch: %v", err)
	}
	if err := fetchThrough(tunnel.server, "proxy", tunnelURL); err != nil {
		t.Fatalf("after the batch: %v", err)
	}
}

func TestLibXrayCoreBuildersRefuseBesideABatchToo(t *testing.T) {
	request := mustJSON(t, map[string]any{
		"apiVersion": apiVersion, "method": libXrayTestXray, "payload": map[string]any{"xrayJson": freedomConfig(t)},
	})
	var refusal string
	site, during := siteCheckingDuringTheBatch(t, func() error {
		refusal = Invoke(request)
		return nil
	})
	if results := pingOne(t, PingRequest{Configs: []PingConfig{{XrayJSON: freedomConfig(t)}}, URL: site.url()}); !results[0].Success {
		t.Fatalf("ping: %+v", results[0])
	}
	if err := during.get(); err != nil {
		t.Fatal(err)
	}
	if response := decodeResponse(t, refusal); response.Success || !strings.Contains(response.Error, "without a running core") {
		t.Fatalf("testXray during a batch = %+v, want a refusal", response)
	}
}

func TestOwnerPrefersThePingCoreThatDialsTheDomain(t *testing.T) {
	tunnelCore := &liveCore{}
	first := &liveCore{hosts: map[string]bool{"a.example": true}}
	second := &liveCore{hosts: map[string]bool{"b.example": true}}
	cores := []*liveCore{first, tunnelCore, second}
	for domain, want := range map[string]*liveCore{"B.Example": second, "a.example": first, "c.example": tunnelCore} {
		if got := owner(cores, domain); got != want {
			t.Errorf("owner(%s) = %p, want %p", domain, got, want)
		}
	}
	if got := owner([]*liveCore{first, second}, "c.example"); got != second {
		t.Errorf("without a tunnel core, owner = %p, want the newest %p", got, second)
	}
}

func TestServerDomainsSkipIPLiterals(t *testing.T) {
	outbound := map[string]any{
		"settings": map[string]any{
			"vnext":   []any{map[string]any{"address": "VLESS.example"}, map[string]any{"address": "1.2.3.4"}},
			"servers": []any{map[string]any{"address": "trojan.example"}},
			"peers":   []any{map[string]any{"endpoint": "wg.example:51820"}, map[string]any{"endpoint": "[2001:db8::1]:51820"}},
			"address": "hy2.example",
		},
		"streamSettings": map[string]any{"xhttpSettings": map[string]any{
			"extra": map[string]any{"downloadSettings": map[string]any{"address": "down.example"}},
		}},
	}
	got := fmt.Sprint(serverDomains(outbound))
	if want := "[hy2.example vless.example trojan.example wg.example down.example]"; got != want {
		t.Fatalf("serverDomains = %s, want %s", got, want)
	}
}
