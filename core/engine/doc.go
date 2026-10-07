// Package engine is the BodoVPN core: XTLS/libXray plus what the apps need in the same Go
// runtime, since a process may load only one. It owns the tunnel's core, so its traffic
// counters are read in process; it times servers warm, through one temporary core per batch
// that can run beside the tunnel's; and it lets Android protect the core's sockets.
package engine
