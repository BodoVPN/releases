# Go standard library patches

`bodocore.yml` applies every `*.patch` here to the Go standard library of the toolchain it builds
with (`bodocore.py patch-go`). The patched files are copies in the job's temp directory, swapped in
through a `-overlay` in `GOFLAGS`; the toolchain itself is never modified. Each patch must apply
cleanly: the step runs `git apply --check` first and fails when a Go bump moves the patched lines,
so a patch is never silently dropped. Paths are relative to `GOROOT`. The patched files are
BSD-licensed like the rest of Go.

A local build needs `GOTOOLCHAIN=local` and the pinned Go installed outside the module cache: go
refuses overlays of files under `GOMODCACHE`, where an auto-downloaded toolchain lives, and the step
says so.

## h2-scratch-32k.patch

The HTTP/2 client gives every request body a scratch buffer of min(the server's advertised max frame
size, 512 KiB) and holds it until the body ends. XHTTP's stream-one and stream-up bodies are the
proxied connection's upload, so each open connection pins its buffer for its whole life. Xray's
XHTTP server builds its `http.Server` without an HTTP/2 config, so it advertises Go's 1 MiB default,
and every stream gets the full 512 KiB. In an iOS Network Extension (about 50 MB before jetsam) a
burst of connections holds tens of MiB, and the runtime's memory limit makes the heap collect back to
back. The patch caps the buffer at 32 KiB, `io.Copy`'s size. Xray hands the body at most one 8 KiB
`buf.Buffer` per read (`buf.Size`; measured in XTLS/Xray-core#6268, comment 5140779704, with the
frame-size tradeoff in golang/go#79831), so the larger buffer never carried more data.

Measured on an iPhone 15 Pro Max (BodoVPN/bodovpn-client#143), VLESS + XHTTP stream-one + REALITY
through a server whose uplink was held to 1 Mbit/s with 100 ms delay and 2% loss, two bursts of 60
parallel HTTPS requests, 90 s per run:

| core | HeapAlloc peak / end | GCs per run |
| --- | --- | --- |
| bodo.3 (control, twice) | 48.3 / 44.3 and 48.9 / 44.3 MiB | 582, 612 |
| xmux `maxConcurrency: 8` only (config) | 22.8 / 20.4 MiB | 307 |
| this patch (a patched toolchain; then this step's overlay) | 15.7 / 15.7 and 15.6 / 15.6 MiB | 2, 2 |
| x/net's legacy transport (`http2legacy`) with the same cap | 15.9 / 10.3 MiB | 3 |

The control's heap profile put 39.7 of 48 MiB in `(*clientStream).writeRequestBody`, in 512 KiB
objects, and 37 MiB was still there 60 s later. Burst times stayed within run-to-run noise.
packet-up bodies carry a Content-Length, which already bounds the buffer: those runs stayed at about
14 MiB on every core. Plain TCP (no HTTP/2) does not use this code.

Throughput is unchanged where we could measure it. On an unthrottled inbound of the same server, one
200 MB stream through the tunnel uploaded at a median 43.6 Mbit/s with the patch (8 runs) against
43.9 (9 runs) without it, and downloaded at about 49.7 either way; download bodies don't use this
buffer. That path tops out near 50 Mbit/s (the phone did 370 up and 413 down without the VPN), so it
says nothing about gigabit rates.

Prior art: XTLS/Xray-core#6268 reported the same buffer on iOS and tested 16 KiB; golang/go#79831
proposes lowering the cap in Go itself. A server advertising a smaller max frame size would also
shrink the buffer, but Xray closed that (XTLS/Xray-core#6663, #6846): it can't reach a CDN, whose own
SETTINGS apply (Cloudflare advertises 16777215, per golang/go#79831), so the cap has to be on the
client.

Drop the patch when the pinned Go caps the buffer itself (golang/go#79831).

## h2-window-growth.patch

The HTTP/2 client advertises a fixed receive window to every response stream: 4 MiB unless a
`net/http` Transport's `HTTP2` config says otherwise, and XHTTP's client (a bare
`golang.org/x/net/http2.Transport`, which on Go 1.27 wraps this client) has no such config. When an
app reads a connection slowly, the tun's send buffer fills, Xray stops reading the response body, and
the server may still send the whole window, which buffers unread in the client. In an iOS Network
Extension (about 50 MB before jetsam), two dozen such streams ended the tunnel every 10-15 s. A fixed
smaller window either stays unsafe (1 MiB) or costs a lone stream a third of its speed (512 KiB);
capping the connection window instead lets stalled streams starve the live ones sharing it, since
connection credit comes back only as the app reads.

The patch auto-tunes the stream window instead, as QUIC stacks and browsers do. A stream starts at
`BODOCORE_H2_STREAM_WINDOW` and doubles, up to `BODOCORE_H2_STREAM_WINDOW_MAX`, each time its reader
takes a whole window's worth within a second: the window, not the reader, is what limits it. A slow
or stalled reader never grows. A granted window can't be taken back, so a stream that grew and then
stalls can still buffer all of it; `BODOCORE_H2_WINDOW_GROWTH_BUDGET` bounds that process-wide, since
growth above every stream's initial window is held from it until the response body is closed or read
to the end. Consumed bytes are always credited back, so a spent budget only stops growth and no
stream starves. All three are read from the environment when a connection is made (a config's root
`env` sets them); unset, the client is stock, so only a platform that sets them changes. The iOS
client sets 256 KiB, 4 MiB and 4 MiB.

Measured on an iPhone 15 Pro Max (BodoVPN/bodovpn-client#211), VLESS + XHTTP stream-one + REALITY to
a throwaway server at about 131 ms round trip:

| client | 24 stalled readers + one 480 KB/s reader, 3 min | one download, 25 s | 8 streams in turn: full speed, then stalled |
| --- | --- | --- | --- |
| stock (4 MiB windows) | killed every 10-15 s | 4.15, 3.78 MB/s | not run |
| fixed 512 KiB | held, 39.4 MiB | 2.49, 2.50, 2.47 MB/s | not run |
| 2 MiB connection window | held, 36.7 MiB; the live reader starved (20 KB/s) | not run | not run |
| this patch, no budget | not run | not run | killed: 3 restarts, 45.6 MiB |
| this patch | held, 36.6 MiB; live reader 496 KB/s | 3.60, 3.79, 3.75 MB/s | held, 36.9 MiB |

`flow_test.go` covers the growth rule, the cap, the budget and its release; `transport_test.go` runs
the client against frames: growth while the reader keeps up, none for a stalled or slow reader, and a
budget two streams share.

