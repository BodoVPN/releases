package engine

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
)

const maxProbeBody = 64 << 10

// probeWarm times target through the outbound tagged tag: a warmup HEAD pays the dial (TCP,
// TLS or REALITY, the proxy handshake), then a second HEAD is timed over the kept-alive
// connection. That is the steady-state number v2rayN and v2rayNG report, not the cold dial.
func probeWarm(ctx context.Context, server *core.Instance, tag, target string, timeout time.Duration) PingResult {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			destination, err := xnet.ParseDestination("tcp:" + address)
			if err != nil {
				return nil, err
			}
			return core.Dial(session.SetForcedOutboundTagToContext(ctx, tag), server, destination)
		},
		MaxIdleConnsPerHost: 1,
		DisableCompression:  true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		// A redirect's own reply is a round trip; following it could open a second connection.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if _, err := timedHead(ctx, client, target, timeout); err != nil {
		return failedPing("warmup", err)
	}
	start := time.Now()
	reused, err := timedHead(ctx, client, target, timeout)
	elapsed := time.Since(start)
	if err != nil {
		return failedPing("timed request", err)
	}
	return PingResult{Success: true, Delay: max(1, elapsed.Milliseconds()), Warm: reused}
}

// timedHead sends one HEAD within timeout and reports whether it rode a kept-alive connection.
func timedHead(ctx context.Context, client *http.Client, target string, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reused := false
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodHead, target, nil)
	if err != nil {
		return false, err
	}
	response, err := client.Do(request)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxProbeBody))
	_ = response.Body.Close()
	return reused, nil
}
