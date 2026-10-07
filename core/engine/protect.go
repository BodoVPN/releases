package engine

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/xtls/xray-core/transport/internet"
)

// ProtectFunc keeps one socket out of the VPN (Android's VpnService.protect); false refuses it.
type ProtectFunc func(fd uintptr) bool

var (
	errProtect = errors.New("protect refused the socket")

	protector       atomic.Pointer[ProtectFunc]
	registerOnce    sync.Once
	registerFailure error
)

// SetProtect hands every socket Xray opens from now on, in any core of this process, to
// protect before it connects; nil stops that. Xray logs a refusal and still dials, as it
// does for any socket controller, so a refused socket is visible in the core's log.
func SetProtect(protect ProtectFunc) error {
	if protect == nil {
		protector.Store(nil)
		return nil
	}
	protector.Store(&protect)
	// Xray keeps its controllers for the life of the process, so ours goes in once.
	registerOnce.Do(func() {
		registerFailure = errors.Join(
			internet.RegisterDialerController(protectSocket),
			internet.RegisterListenerController(protectSocket),
		)
	})
	return registerFailure
}

func protectSocket(network, address string, conn syscall.RawConn) error {
	protect := protector.Load()
	if protect == nil {
		return nil
	}
	accepted := false
	if err := conn.Control(func(fd uintptr) { accepted = (*protect)(fd) }); err != nil {
		return err
	}
	if !accepted {
		return fmt.Errorf("%w: %s %s", errProtect, network, address)
	}
	return nil
}
