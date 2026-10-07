package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xerrors "github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/core"
)

func pingOne(t *testing.T, request PingRequest) []PingResult {
	t.Helper()
	results, err := PingBatchWarm(context.Background(), request, nil)
	if err != nil {
		t.Fatalf("PingBatchWarm: %v", err)
	}
	return results
}

func TestWarmPingTimesTheKeptAliveConnection(t *testing.T) {
	site := newTarget(t, nil)
	results := pingOne(t, PingRequest{Configs: []PingConfig{{XrayJSON: freedomConfig(t)}}, URL: site.url()})
	if got := results[0]; !got.Success || !got.Warm || got.Delay < 1 || got.Error != "" {
		t.Fatalf("result = %+v, want a warm success", got)
	}
	if site.connections.Load() != 1 || site.requests.Load() != 2 {
		t.Fatalf("site saw %d connections and %d requests, want 1 and 2", site.connections.Load(), site.requests.Load())
	}
}

func TestWarmPingThroughARealProxy(t *testing.T) {
	const id = "27848739-7e62-4138-9fd3-098a63964b6b"
	site := newTarget(t, nil)
	port := startServerCore(t, id)
	results := pingOne(t, PingRequest{Configs: []PingConfig{{XrayJSON: vlessClientConfig(t, port, id)}}, URL: site.url()})
	if got := results[0]; !got.Success || !got.Warm {
		t.Fatalf("result = %+v, want a warm success through VLESS", got)
	}
	if site.connections.Load() != 1 {
		t.Fatalf("site saw %d connections, want the warmup's one reused", site.connections.Load())
	}
}

func TestWarmPingFailsOnlyTheBadRows(t *testing.T) {
	site := newTarget(t, nil)
	var rows recorder
	request := PingRequest{
		Configs: []PingConfig{
			{XrayJSON: freedomConfig(t)},
			{XrayJSON: blackholeConfig(t)},
			{XrayJSON: "not json"},
			{XrayJSON: freedomConfig(t), OutboundTag: "missing"},
			{XrayJSON: `{"outbounds":[{"tag":"proxy","protocol":"nope"}]}`},
			{XrayJSON: freedomConfig(t), OutboundTag: "direct"},
		},
		URL:       site.url(),
		TimeoutMs: 2000,
	}
	results, err := PingBatchWarm(context.Background(), request, rows.record)
	if err != nil {
		t.Fatal(err)
	}
	wantOK := []bool{true, false, false, false, false, true}
	wantError := []string{"", "warmup:", "config: the config is not a JSON object", `config: no outbound is tagged "missing"`, `config: outbound "proxy"`, ""}
	for i, result := range results {
		if result.Success != wantOK[i] || !strings.HasPrefix(result.Error, wantError[i]) {
			t.Fatalf("row %d = %+v, want success %v and error %q\n%s", i, result, wantOK[i], wantError[i], describe(results))
		}
		if !result.Success && result.Delay != FailedDelay {
			t.Fatalf("row %d delay = %d, want %d", i, result.Delay, FailedDelay)
		}
		if rows.calls[i] != result {
			t.Fatalf("row %d streamed %+v, returned %+v", i, rows.calls[i], result)
		}
	}
	if rows.count != len(request.Configs) {
		t.Fatalf("onResult ran %d times, want once per row", rows.count)
	}
}

func TestWarmPingGivesEachConfigItsOwnCoreWhenTheMergedOneFails(t *testing.T) {
	site := newTarget(t, nil)
	original := startPingCore
	t.Cleanup(func() { startPingCore = original })
	var merged atomic.Int64
	startPingCore = func(items []pingItem) (*core.Instance, error) {
		if len(items) > 1 {
			merged.Add(1)
			return nil, errors.New("refused")
		}
		return original(items)
	}
	results := pingOne(t, PingRequest{
		Configs: []PingConfig{{XrayJSON: freedomConfig(t)}, {XrayJSON: freedomConfig(t)}, {XrayJSON: blackholeConfig(t)}},
		URL:     site.url(), TimeoutMs: 2000,
	})
	if merged.Load() != 1 || !results[0].Success || !results[1].Success || results[2].Success {
		t.Fatalf("merged attempts %d, results:\n%s", merged.Load(), describe(results))
	}
}

func TestWarmPingProbesConcurrentlyUpToTheLimit(t *testing.T) {
	configs := []PingConfig{}
	for range 4 {
		configs = append(configs, PingConfig{XrayJSON: freedomConfig(t)})
	}
	serial := newTarget(t, func(s *target) { s.delay = 100 * time.Millisecond })
	pingOne(t, PingRequest{Configs: configs, URL: serial.url(), Concurrency: 1})
	if peak := serial.peak.Load(); peak != 1 {
		t.Fatalf("concurrency 1 reached %d requests at once", peak)
	}
	parallel := newTarget(t, func(s *target) { s.delay = 100 * time.Millisecond })
	pingOne(t, PingRequest{Configs: configs, URL: parallel.url(), Concurrency: 4})
	if peak := parallel.peak.Load(); peak < 2 {
		t.Fatalf("concurrency 4 reached only %d requests at once", peak)
	}
}

func TestWarmPingTimesOutAStalledServer(t *testing.T) {
	site := newTarget(t, func(s *target) { s.hang = make(chan struct{}) })
	start := time.Now()
	results := pingOne(t, PingRequest{Configs: []PingConfig{{XrayJSON: freedomConfig(t)}}, URL: site.url(), TimeoutMs: 300})
	if got := results[0]; got.Success || !strings.Contains(got.Error, "deadline exceeded") {
		t.Fatalf("result = %+v, want a timeout", got)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("a 300 ms timeout took %v", elapsed)
	}
}

// handlerSpy stands in for a running tunnel core's log handler.
type handlerSpy struct{ messages atomic.Int64 }

func (h *handlerSpy) Handle(log.Message) { h.messages.Add(1) }

func TestWarmPingLeavesARunningTunnelCoreAlone(t *testing.T) {
	stopTunnel(t)
	site := newTarget(t, nil)
	if err := tunnel.run(statsConfig(t)); err != nil {
		t.Fatal(err)
	}
	spy := &handlerSpy{}
	log.RegisterHandler(spy)

	results := pingOne(t, PingRequest{Configs: []PingConfig{{XrayJSON: freedomConfig(t)}}, URL: site.url()})
	if !results[0].Success {
		t.Fatalf("ping beside the tunnel core: %+v", results[0])
	}

	xerrors.LogWarning(context.Background(), "after the batch")
	if spy.messages.Load() == 0 {
		t.Fatal("the batch replaced the process's log handler")
	}
	getThrough(t, tunnel.server, "proxy", site.url())
	if _, err := tunnel.counters(); err != nil {
		t.Fatalf("tunnel counters after the batch: %v", err)
	}
}

func TestWarmPingRefusesABadRequest(t *testing.T) {
	good := []PingConfig{{XrayJSON: "{}"}}
	for name, request := range map[string]PingRequest{
		"no configs":     {URL: "https://example.com"},
		"relative url":   {Configs: good, URL: "/generate_204"},
		"ftp url":        {Configs: good, URL: "ftp://example.com"},
		"negative wait":  {Configs: good, URL: "https://example.com", TimeoutMs: -1},
		"too many tries": {Configs: good, URL: "https://example.com", Concurrency: maxPingConcurrency + 1},
	} {
		if _, err := PingBatchWarm(context.Background(), request, nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestPingBatchWarmAnswersInTheInvokeEnvelope(t *testing.T) {
	site := newTarget(t, nil)
	response := invokeFor(t, MethodPingBatchWarm, PingRequest{Configs: []PingConfig{{XrayJSON: freedomConfig(t)}}, URL: site.url()})
	if !response.Success {
		t.Fatalf("pingBatchWarm: %s", response.Error)
	}
	if results := dataAs[PingResponse](t, response).Results; len(results) != 1 || !results[0].Success {
		t.Fatalf("results = %+v", results)
	}
	if response := invokeFor(t, MethodPingBatchWarm, map[string]any{"url": site.url()}); response.Success {
		t.Fatal("a payload without configs succeeded")
	}
}
