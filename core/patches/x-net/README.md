# x/net patch (PROTOTYPE, not wired into bodocore.yml)

`frame-scratch-32k.patch` caps x/net's HTTP/2 client request-body scratch buffer
(`frameScratchBufferLen`) at 32 KiB instead of 512 KiB (BodoVPN/bodovpn-client#143).

Why: on iOS, a burst of about 60 XHTTP sessions (stream-one over REALITY) took the Go heap from
23 to 53.6 MiB and kept it live: 512 KiB per upload stream, mostly untouched, so the footprint
lagged until traffic touched the pages and the extension was killed. Body reads never exceed
8 KiB (`buf.Size`), so 32 KiB costs no throughput (XTLS/Xray-core#6268, golang/go#79831).

On Go 1.27, x/net's own `http2/transport.go` is built only with `-tags http2legacy`; otherwise
x/net wraps net/http's internal h2, where the same constant lives in the standard library. So
this patch needs the `http2legacy` tag, which also restores x/net's connection pooling
(XTLS/Xray-core#6797: `xmux.maxConnections` does nothing on Go 1.27 builds).

Prototype status (2026-10-12): built by hand (patched copy, `go mod edit -replace`, the tag on
the Apple slices). In one burst on an iPhone 15 Pro Max, HeapAlloc stayed at 17.7 MiB at most
(against 53.6), but the first burst took 25.5 s and the session was then ended by the watchdog.
That run coincided with the home-to-server path failing (the server was healthy from elsewhere),
so the stall is unconfirmed. Next: (a) http2legacy alone, (b) this cap applied to the standard
library's h2 through `-overlay` (no legacy pooling), (c) legacy with a higher xmux concurrency,
each with a master control run in the same hour, then the HTTP2Config receive caps.

To wire it: extend `bodocore.py patch-xray` to patch a second module (golang.org/x/net) and add
`http2legacy` to every build's tags. Drop it, the tag and the gVisor patch once upstream fixes
XTLS/Xray-core#6268/#6797 and #7130 ships.
