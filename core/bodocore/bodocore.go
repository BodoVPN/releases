// Package bodocore is the core's gomobile binding: class com.bodovpn.bodocore.Bodocore in the
// Android AAR. It is the only Go library an app process loads; libXray is inside it.
package bodocore

import "github.com/bodovpn/releases/core/engine"

// Invoke answers one JSON request in libXray's envelope; see engine.Invoke.
func Invoke(request string) string {
	return engine.Invoke(request)
}

// PingListener gets each warm ping result, as the JSON of engine.PingResult, as it lands.
// Calls come from Go threads, one at a time.
type PingListener interface {
	OnResult(index int, resultJSON string)
}

// PingBatchWarm runs pingBatchWarm (payload: engine.PingRequest) and streams each row to
// listener (may be null); it returns the same envelope Invoke would.
func PingBatchWarm(payload string, listener PingListener) string {
	return engine.PingBatchWarmJSON(payload, func(index int, result engine.PingResult) {
		if listener != nil {
			listener.OnResult(index, result.JSON())
		}
	})
}

// Protector is Android's VpnService.protect for one socket's fd; false refuses the socket.
type Protector interface {
	Protect(fd int) bool
}

// SetProtector hands every socket the core opens from now on to protector; null stops that.
func SetProtector(protector Protector) error {
	if protector == nil {
		return engine.SetProtect(nil)
	}
	return engine.SetProtect(func(fd uintptr) bool { return protector.Protect(int(fd)) })
}
