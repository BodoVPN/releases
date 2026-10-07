//go:build windows || (linux && !android)

// Command bodocore is the desktop core: libXray's desktop `xray run`, plus the tunnel core's
// traffic counters on stdout (no listener) and the warm ping batch, out of the app's process.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bodovpn/releases/core/engine"
	"github.com/xtls/libxray/dns"
)

func main() {
	os.Exit(execute(os.Args[1:], commands{run: run, ping: ping}, os.Stdin, os.Stdout, os.Stderr))
}

// lines writes whole lines to the parent, one at a time.
type lines struct {
	mu  sync.Mutex
	out io.Writer
}

func (l *lines) write(prefix string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = fmt.Fprintf(l.out, "%s%s\n", prefix, raw)
	return err
}

// run is libXray's `xray run`: the core's own lookups bound to the uplink, the core until
// SIGINT or SIGTERM. With -stats-interval it also writes its traffic counters that often.
func run(opts options, stdout io.Writer) error {
	config, err := os.ReadFile(opts.configPath)
	if err != nil {
		return err
	}
	text := string(config)
	if opts.statsInterval > 0 {
		if text, err = engine.WithTrafficCounters(text); err != nil {
			return err
		}
	}
	if err := dns.SetDNS(opts.dns, opts.interfaceName); err != nil {
		return err
	}
	defer dns.ResetDNS()
	if err := engine.RunTunnel(text); err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if opts.statsInterval > 0 {
		ticker := time.NewTicker(opts.statsInterval)
		defer ticker.Stop()
		out := &lines{out: stdout}
		for {
			select {
			case <-signals:
				return engine.StopTunnel()
			case <-ticker.C:
				if err := writeCounters(out); err != nil {
					_ = engine.StopTunnel()
					return err
				}
			}
		}
	}
	<-signals
	return engine.StopTunnel()
}

func writeCounters(out *lines) error {
	counters, err := engine.TunnelCounters()
	if err != nil {
		return fmt.Errorf("traffic counters: %w", err)
	}
	return out.write(statsPrefix, counters)
}

// pingRow is one streamed result, with the index of its config in the request.
type pingRow struct {
	Index int `json:"index"`
	engine.PingResult
}

// ping reads a PingRequest from stdin, writes one line per row as it lands, and exits.
func ping(opts options, stdin io.Reader, stdout io.Writer) error {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	var request engine.PingRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("ping request: %w", err)
	}
	if err := dns.SetDNS(opts.dns, opts.interfaceName); err != nil {
		return err
	}
	defer dns.ResetDNS()
	out := &lines{out: stdout}
	var writeErr error
	_, err = engine.PingBatchWarm(context.Background(), request, func(index int, result engine.PingResult) {
		if err := out.write(pingPrefix, pingRow{Index: index, PingResult: result}); err != nil && writeErr == nil {
			writeErr = err
		}
	})
	if err != nil {
		return err
	}
	return writeErr
}
