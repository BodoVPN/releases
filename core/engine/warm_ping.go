package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/xtls/xray-core/core"
)

const (
	// FailedDelay is a failed row's delay, as the apps already read it.
	FailedDelay int64 = -1

	DefaultPingTimeoutMs   = 5000
	DefaultPingConcurrency = 8
	maxPingConfigs         = 1000
	maxPingConcurrency     = 64
	maxPingTimeoutMs       = 60000
)

// PingRequest asks for the real delay of every config, through ONE temporary core.
type PingRequest struct {
	Configs []PingConfig `json:"configs"`
	// URL is requested twice per config: a warmup that pays the dial, then the timed request.
	URL string `json:"url"`
	// TimeoutMs bounds each of the two requests; 0 means DefaultPingTimeoutMs.
	TimeoutMs int `json:"timeoutMs,omitempty"`
	// Concurrency is how many configs are probed at once; 0 means DefaultPingConcurrency.
	Concurrency int `json:"concurrency,omitempty"`
}

// PingConfig is one server: a full Xray config and the outbound to time ("proxy", else the
// first, when OutboundTag is empty). Its inbounds, log, stats, API and env are never used.
type PingConfig struct {
	XrayJSON    string `json:"xrayJson"`
	OutboundTag string `json:"outboundTag,omitempty"`
}

// PingResult is one config's outcome. Warm is false when the server closed the warmup's
// connection, so the timed request had to dial again.
type PingResult struct {
	Success bool   `json:"success"`
	Delay   int64  `json:"delay"`
	Warm    bool   `json:"warm"`
	Error   string `json:"error,omitempty"`
}

// PingResponse holds one result per config, in request order.
type PingResponse struct {
	Results []PingResult `json:"results"`
}

// JSON is the result as one JSON object, the form the bindings stream.
func (r PingResult) JSON() string {
	raw, err := json.Marshal(r)
	if err != nil {
		return `{"success":false,"delay":-1,"warm":false,"error":"failed to encode result"}`
	}
	return string(raw)
}

func failedPing(stage string, err error) PingResult {
	return PingResult{Delay: FailedDelay, Error: fmt.Sprintf("%s: %v", stage, err)}
}

// PingBatchWarmJSON decodes a PingRequest payload, runs PingBatchWarm and encodes the answer in
// Invoke's envelope; onResult, when set, gets each result as it lands.
func PingBatchWarmJSON(payload string, onResult func(int, PingResult)) string {
	var request PingRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		return encode(nil, fmt.Errorf("pingBatchWarm payload: %w", err))
	}
	results, err := PingBatchWarm(context.Background(), request, onResult)
	if err != nil {
		return encode(nil, err)
	}
	return encode(PingResponse{Results: results}, nil)
}

// PingBatchWarm times every config warm through one temporary core, concurrently. The core
// has no inbound, log or env, so it never listens and leaves a running tunnel core alone.
// A bad config fails only its own row; when Xray refuses the merged core, each config gets
// a core of its own. onResult, when set, is called once per row as it lands, one at a time.
func PingBatchWarm(ctx context.Context, request PingRequest, onResult func(int, PingResult)) ([]PingResult, error) {
	timeout, workers, err := request.validate()
	if err != nil {
		return nil, err
	}
	rows := &rowReporter{results: make([]PingResult, len(request.Configs)), onResult: onResult}
	var items []pingItem
	for index, config := range request.Configs {
		item, err := planPingItem(index, config)
		if err != nil {
			rows.report(index, failedPing("config", err))
			continue
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return rows.results, nil
	}
	server, err := startPingCore(items)
	if err != nil {
		forEachItem(items, workers, rows, func(item pingItem) PingResult {
			return probeAlone(ctx, item, request.URL, timeout)
		})
		return rows.results, nil
	}
	defer server.Close()
	forEachItem(items, workers, rows, func(item pingItem) PingResult {
		return probeWarm(ctx, server, item.tag, request.URL, timeout)
	})
	return rows.results, nil
}

func (r PingRequest) validate() (time.Duration, int, error) {
	if len(r.Configs) == 0 {
		return 0, 0, errors.New("pingBatchWarm needs at least one config")
	}
	if len(r.Configs) > maxPingConfigs {
		return 0, 0, fmt.Errorf("pingBatchWarm takes at most %d configs", maxPingConfigs)
	}
	target, err := url.ParseRequestURI(r.URL)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return 0, 0, errors.New("pingBatchWarm url must be an absolute http or https URL")
	}
	timeoutMs := r.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = DefaultPingTimeoutMs
	}
	if timeoutMs < 0 || timeoutMs > maxPingTimeoutMs {
		return 0, 0, fmt.Errorf("pingBatchWarm timeoutMs must be 1-%d", maxPingTimeoutMs)
	}
	workers := r.Concurrency
	if workers == 0 {
		workers = DefaultPingConcurrency
	}
	if workers < 0 || workers > maxPingConcurrency {
		return 0, 0, fmt.Errorf("pingBatchWarm concurrency must be 1-%d", maxPingConcurrency)
	}
	return time.Duration(timeoutMs) * time.Millisecond, workers, nil
}

// probeAlone times one config through a core of its own, for when the merged core failed.
func probeAlone(ctx context.Context, item pingItem, target string, timeout time.Duration) PingResult {
	server, err := startPingCore([]pingItem{item})
	if err != nil {
		return failedPing("core", err)
	}
	defer server.Close()
	return probeWarm(ctx, server, item.tag, target, timeout)
}

type rowReporter struct {
	mu       sync.Mutex
	results  []PingResult
	onResult func(int, PingResult)
}

func (r *rowReporter) report(index int, result PingResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results[index] = result
	if r.onResult != nil {
		r.onResult(index, result)
	}
}

// forEachItem probes the items on up to workers goroutines. A panic in a probe fails its row
// instead of the process, as far as it unwinds through this goroutine.
func forEachItem(items []pingItem, workers int, rows *rowReporter, probe func(pingItem) PingResult) {
	jobs := make(chan pingItem)
	var wg sync.WaitGroup
	for range min(workers, len(items)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				rows.report(item.index, guardedProbe(item, probe))
			}
		}()
	}
	for _, item := range items {
		jobs <- item
	}
	close(jobs)
	wg.Wait()
}

func guardedProbe(item pingItem, probe func(pingItem) PingResult) (result PingResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = failedPing("probe", fmt.Errorf("panic: %v", recovered))
		}
	}()
	return probe(item)
}

// startPingCore is a variable so a test can make Xray refuse the merged core.
var startPingCore = func(items []pingItem) (*core.Instance, error) {
	config, err := buildPingCoreConfig(items)
	if err != nil {
		return nil, err
	}
	server, err := core.New(config)
	if err != nil {
		return nil, err
	}
	if err := server.Start(); err != nil {
		_ = server.Close()
		return nil, err
	}
	return server, nil
}
