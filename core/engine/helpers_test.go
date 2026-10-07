package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
)

// target is a local HTTP server that counts the connections and requests it gets.
type target struct {
	*httptest.Server
	connections atomic.Int64
	requests    atomic.Int64
	inFlight    atomic.Int64
	peak        atomic.Int64
	delay       time.Duration
	hang        chan struct{}
	// onRequest runs inside the handler with the request's number, from 1.
	onRequest func(int64)
}

func newTarget(t *testing.T, configure func(*target)) *target {
	t.Helper()
	server := &target{}
	if configure != nil {
		configure(server)
	}
	server.Server = httptest.NewUnstartedServer(http.HandlerFunc(server.serve))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			server.connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(func() {
		if server.hang != nil {
			close(server.hang)
		}
		server.Close()
	})
	return server
}

func (s *target) serve(w http.ResponseWriter, _ *http.Request) {
	number := s.requests.Add(1)
	if s.onRequest != nil {
		s.onRequest(number)
	}
	now := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		peak := s.peak.Load()
		if now <= peak || s.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	if s.hang != nil {
		<-s.hang
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *target) url() string {
	return s.URL + "/generate_204"
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func freedomConfig(t *testing.T) string {
	t.Helper()
	return mustJSON(t, map[string]any{
		"outbounds": []any{
			map[string]any{"tag": "proxy", "protocol": "freedom"},
			map[string]any{"tag": "direct", "protocol": "freedom"},
		},
	})
}

func blackholeConfig(t *testing.T) string {
	t.Helper()
	return mustJSON(t, map[string]any{
		"outbounds": []any{map[string]any{"tag": "proxy", "protocol": "blackhole"}},
	})
}

// statsConfig is a tunnel config that counts the proxy outbound's traffic.
func statsConfig(t *testing.T) string {
	t.Helper()
	return mustJSON(t, map[string]any{
		"log":       map[string]any{"loglevel": "none"},
		"outbounds": []any{map[string]any{"tag": "proxy", "protocol": "freedom"}},
		"stats":     map[string]any{},
		"policy": map[string]any{"system": map[string]any{
			"statsOutboundUplink": true, "statsOutboundDownlink": true,
		}},
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// startServerCore runs a VLESS server core on loopback whose freedom may reach loopback.
func startServerCore(t *testing.T, id string) int {
	t.Helper()
	port := freePort(t)
	config := mustJSON(t, map[string]any{
		"log": map[string]any{"loglevel": "none"},
		"inbounds": []any{map[string]any{
			"listen": "127.0.0.1", "port": port, "protocol": "vless",
			"settings": map[string]any{"clients": []any{map[string]any{"id": id}}, "decryption": "none"},
		}},
		"outbounds": []any{map[string]any{
			"protocol": "freedom",
			"settings": map[string]any{"finalRules": []any{
				map[string]any{"action": "allow", "ip": []any{"127.0.0.0/8"}},
			}},
		}},
	})
	loaded, err := core.LoadConfig("json", strings.NewReader(config))
	if err != nil {
		t.Fatal(err)
	}
	server, err := core.New(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return port
}

func vlessClientConfig(t *testing.T, port int, id string) string {
	t.Helper()
	return mustJSON(t, map[string]any{
		"outbounds": []any{map[string]any{
			"tag": "proxy", "protocol": "vless",
			"settings": map[string]any{"vnext": []any{map[string]any{
				"address": "127.0.0.1", "port": port,
				"users": []any{map[string]any{"id": id, "encryption": "none"}},
			}}},
		}},
	})
}

// getThrough fetches url through the running tunnel core's outbound tag.
func getThrough(t *testing.T, server *core.Instance, tag, url string) {
	t.Helper()
	if err := fetchThrough(server, tag, url); err != nil {
		t.Fatalf("GET through %s: %v", tag, err)
	}
}

func fetchThrough(server *core.Instance, tag, url string) error {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			destination, err := xnet.ParseDestination("tcp:" + address)
			if err != nil {
				return nil, err
			}
			return core.Dial(session.SetForcedOutboundTagToContext(ctx, tag), server, destination)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return response.Body.Close()
}

func invokeFor(t *testing.T, method string, payload any) invokeResponse {
	t.Helper()
	request := map[string]any{"apiVersion": apiVersion, "method": method}
	if payload != nil {
		request["payload"] = payload
	}
	return decodeResponse(t, Invoke(mustJSON(t, request)))
}

func decodeResponse(t *testing.T, raw string) invokeResponse {
	t.Helper()
	var response invokeResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("not an envelope: %s", raw)
	}
	return response
}

func dataAs[T any](t *testing.T, response invokeResponse) T {
	t.Helper()
	var value T
	raw, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("data %s: %v", raw, err)
	}
	return value
}

// stopTunnel stops the package's tunnel core at the end of a test that started it.
func stopTunnel(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := tunnel.stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
}

// recorder collects onResult calls.
type recorder struct {
	mu    sync.Mutex
	calls map[int]PingResult
	count int
}

func (r *recorder) record(index int, result PingResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[int]PingResult{}
	}
	r.calls[index] = result
	r.count++
}

func describe(results []PingResult) string {
	var lines []string
	for i, result := range results {
		lines = append(lines, fmt.Sprintf("%d: %+v", i, result))
	}
	return strings.Join(lines, "\n")
}
