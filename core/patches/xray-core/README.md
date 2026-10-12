# Xray-core patches

`bodocore.yml` applies every `*.patch` here to the Xray-core version `core/go.mod` pins before it
builds or tests anything (`bodocore.py patch-xray`). Each patch must apply cleanly: the build runs
`git apply --check` first and fails when an upstream bump moves the patched lines, so a patch is
never silently dropped. The patched files are MPL-2.0 like the rest of Xray-core; the patches stay
public here.

## gvisor-tcp-buffers.patch

Xray's `tun` inbound (gVisor netstack) sizes each TCP connection's buffers at a 1 MiB default and
up to 8 MiB receive / 6 MiB send. The link between the OS and the netstack has a near-zero round
trip, so its bandwidth-delay product is tiny: 128 KiB default and 256 KiB max per direction still
carry more than 100 MB/s per connection. In a memory-capped process, an iOS Network Extension
(about 50 MB before jetsam), the larger buffers let a burst of parallel connections push the Go heap
into the runtime's memory limit, where it collects back to back.

Measured on an iPhone 15 Pro Max over a 15-minute workload, four cycles in one session
(BodoVPN/bodovpn-client#195, comment 6133017236): with 2 MiB / 1 MiB buffers and a 36 MiB limit, the
first cycle's download ran 266 GC/s at half the throughput; with this patch plus our iOS runtime
policy (`engine/runtime_policy.go`), every cycle stayed in low mode at about 20% less CPU than the C
packet bridge it replaced. (The 334 GC/s in an earlier #195 comment is libXray's original policy.)

Android runs the same gVisor tun and gets the same sizes; Windows and Linux use tun2socks and do not
load this code.

Prior art: the themoein100/Xray-core `ios-patches-26.9.9` branch reached the same sizes from device
measurements. This patch is our own; it changes only the four constants.

Drop it when the pin moves past XTLS/Xray-core#7130 (merged 2026-10-12, not yet released at the
time of writing): it replaces the tun inbound's gVisor stack with Xray's own TCP/IP stack, whose
per-connection window is the same 256 KiB, so there is nothing left to patch. That stack needs its
own device soak before the bump.
