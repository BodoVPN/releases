# quic-go patches

`bodocore.yml` applies every `*.patch` here to the `github.com/apernet/quic-go` that Xray's Hysteria
transport uses (`bodocore.py patch-quic`), the same way `patch-xray` patches Xray: a copy of the
pinned module in the job's temp directory, checked with `git apply --check` before anything is
applied, then `go mod edit -replace`d. A quic-go bump that moves the patched lines fails the step, so
a patch is never silently dropped. The patched files are MIT-licensed like the rest of quic-go.

## stream-memory.patch

A Hysteria2 TCP connection is one QUIC stream, and every stream a client opens gets the receive
window it advertised. When an app reads a connection slowly, the tun's send buffer fills, Xray stops
reading the stream, and the server may still send the whole window, which buffers unread in the
client. Connection credit comes back only as the app reads, so those stalled streams also use up the
connection window, and every other stream on the connection stops: in an iOS Network Extension
(about 50 MB before jetsam) 24 stalled streams held about 15 MiB unread, ran the footprint into
memory pressure, and starved the one stream still being read to about 20 KB/s.

Smaller stream windows fix the stalled streams (`streamSettings.finalmask.quicParams`), and quic-go
already auto-tunes them up for a fast reader. Two things still leak past them, and the patch closes
both. Each is off unless its variable is set, so only a platform that sets them changes; Xray reads
a config's root `env` into the environment before it dials.

- **Growth budget** (`BODOCORE_QUIC_STREAM_GROWTH_BUDGET`, bytes). A window granted to the peer can't
  be taken back, so a stream that grew while its reader kept up and then stalls can still buffer all
  of it. Growth above each stream's initial window is held from a process-wide budget and returned
  when the stream's receive side is done; a spent budget only stops growth, and no stream starves.
  quic-go grows a window that is read within a few round trips, and a loaded link inflates the round
  trip enough to grow even a slow reader's, so with the budget set a window also has to be read at
  `BODOCORE_QUIC_GROW_MIN_RATE` (bytes per second, 64 KiB/s by default) to grow.
- **Frame compaction** (`BODOCORE_QUIC_COMPACT_STREAM_FRAMES=1`). A received STREAM frame keeps its
  whole 1452-byte pooled packet buffer until the data is read, so a stalled stream held several times
  its window in buffers. A frame that is mostly empty, or that queues behind unread data, has its
  payload copied out and its buffer returned; a stream read as data arrives keeps its frames.

Xray's `ChromeParrot` (on unless `quicParams.disableChromeParrot`) pins quic-go's windows to Chrome's,
6 MiB per stream and 15 MiB per connection, whatever `quicParams` asks for, and they never auto-tune.
The stream credit travels in every handshake's transport parameters and applies to each new stream,
so capping later window updates can't bound it: on a Mac, with the handshake kept and every later
update capped (6 MiB per connection), unread data stayed near 6 MiB but the live stream still starved
at 23 KB/s. These settings only take effect with the parrot off.

Measured on an iPhone 15 Pro Max (BodoVPN/bodovpn-client#211): Hysteria2 to a throwaway
Xray 26.9.30 server at about 131 ms round trip, BBR on the server. The client's windows were
`initStreamReceiveWindow` 128 KiB and `maxStreamReceiveWindow` 4 MiB; "patch" is a 4 MiB budget and
compaction.

| client | 24 stalled readers + one 480 KB/s reader, 3 min | one download, 25 s | 16 downloads in turn: full speed, then stalled |
| --- | --- | --- | --- |
| parrot on (Chrome's windows), no patch | not run | 3.75, 3.67, 3.53 MB/s | not run |
| parrot on, patch | peak 43.5 MiB, 2 pressure events; live reader 23 KB/s | 3.77, 3.72, 3.90 MB/s | not run |
| parrot off, no patch | peak 37.2 MiB; live reader 492 KB/s | not run | peak 45.5 MiB, 6 pressure events |
| **parrot off, patch** | **peak 37.0 MiB, 0 pressure events; live reader 486 KB/s** | **3.80, 3.82, 3.58 MB/s** | **peak 37.4 MiB, 0 pressure events** |

`flow_controller_stream_test.go` covers the budget, its release, its absence (stock) and the
slow-reader rule; `receive_stream_test.go` covers compaction and its absence.
