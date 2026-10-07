// Command cbridge is the core's C library (libBodoCore): c-shared on Windows and Linux,
// c-archive in the Apple xcframework. CGoInvoke and CGoFree keep libXray's C names.
package main

/*
#include <stdint.h>
#include <stdlib.h>

// Receives one warm ping row as JSON; the string is freed when the call returns.
typedef void (*bodo_ping_result_fn)(void *context, int32_t index, const char *result_json);
*/
import "C"

import (
	"unsafe"

	"github.com/bodovpn/releases/core/engine"
)

func main() {}

// CGoInvoke answers one JSON request; free the reply with CGoFree.
//
//export CGoInvoke
func CGoInvoke(request *C.char) *C.char {
	return C.CString(engine.Invoke(C.GoString(request)))
}

// CGoFree frees a string this library returned.
//
//export CGoFree
func CGoFree(value *C.char) {
	C.free(unsafe.Pointer(value))
}

// BodoPingBatchWarm runs pingBatchWarm on a PingRequest payload, calling onResult (may be
// NULL) from Go threads, one row at a time, with context. Free the reply with CGoFree.
//
//export BodoPingBatchWarm
func BodoPingBatchWarm(payload *C.char, onResult C.bodo_ping_result_fn, context unsafe.Pointer) *C.char {
	response := engine.PingBatchWarmJSON(C.GoString(payload), func(index int, result engine.PingResult) {
		if onResult != nil {
			deliverPingResult(onResult, context, index, result.JSON())
		}
	})
	return C.CString(response)
}
