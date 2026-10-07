package engine

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

func TestProtectSeesTheCoresSockets(t *testing.T) {
	stopTunnel(t)
	t.Cleanup(func() { _ = SetProtect(nil) })
	site := newTarget(t, nil)
	var seen atomic.Int64
	var lastFD atomic.Uint64
	if err := SetProtect(func(fd uintptr) bool {
		seen.Add(1)
		lastFD.Store(uint64(fd))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.run(statsConfig(t)); err != nil {
		t.Fatal(err)
	}
	getThrough(t, tunnel.server, "proxy", site.url())
	if seen.Load() == 0 || lastFD.Load() == 0 {
		t.Fatal("protect never saw the tunnel core's dial")
	}

	results := pingOne(t, PingRequest{Configs: []PingConfig{{XrayJSON: freedomConfig(t)}}, URL: site.url()})
	if !results[0].Success {
		t.Fatalf("ping: %+v", results[0])
	}
	afterPing := seen.Load()

	if err := SetProtect(nil); err != nil {
		t.Fatal(err)
	}
	getThrough(t, tunnel.server, "proxy", site.url()+"?again")
	if seen.Load() != afterPing {
		t.Fatal("protect still ran after SetProtect(nil)")
	}
}

func TestProtectRefusalIsAnError(t *testing.T) {
	t.Cleanup(func() { _ = SetProtect(nil) })
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	raw, err := listener.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetProtect(func(uintptr) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if err := protectSocket("tcp", "1.2.3.4:443", raw); !errors.Is(err, errProtect) {
		t.Fatalf("refused socket = %v, want errProtect", err)
	}
	if err := SetProtect(nil); err != nil {
		t.Fatal(err)
	}
	if err := protectSocket("tcp", "1.2.3.4:443", raw); err != nil {
		t.Fatalf("no protector = %v, want nil", err)
	}
}
