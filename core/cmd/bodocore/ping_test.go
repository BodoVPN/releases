//go:build windows || (linux && !android)

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// uplink is an active non-loopback interface with an IPv4 address, as -interface needs.
func uplink(t *testing.T) string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			if ip, ok := address.(*net.IPNet); ok && ip.IP.To4() != nil {
				return iface.Name
			}
		}
	}
	t.Skip("no active IPv4 interface to bind the core's lookups to")
	return ""
}

func TestPingStreamsEveryRowAsALine(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer site.Close()
	request, _ := json.Marshal(map[string]any{
		"configs": []any{
			map[string]any{"xrayJson": `{"outbounds":[{"tag":"proxy","protocol":"freedom"}]}`},
			map[string]any{"xrayJson": `{"outbounds":[{"tag":"proxy","protocol":"blackhole"}]}`},
		},
		"url":       site.URL + "/generate_204",
		"timeoutMs": 2000,
	})
	var stdout bytes.Buffer
	opts := options{command: "ping", dns: "8.8.8.8:53", interfaceName: uplink(t)}
	if err := ping(opts, bytes.NewReader(request), &stdout); err != nil {
		t.Fatal(err)
	}
	rows := map[int]pingRow{}
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, pingPrefix) {
			continue
		}
		var row pingRow
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, pingPrefix)), &row); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		rows[row.Index] = row
	}
	if len(rows) != 2 || !rows[0].Success || !rows[0].Warm || rows[1].Success || rows[1].Delay != -1 {
		t.Fatalf("rows = %+v\nstdout:\n%s", rows, stdout.String())
	}
}

func TestPingRefusesABrokenRequest(t *testing.T) {
	opts := options{command: "ping", dns: "8.8.8.8:53", interfaceName: "eth0"}
	if err := ping(opts, strings.NewReader("{"), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "ping request") {
		t.Fatalf("err = %v", err)
	}
}
