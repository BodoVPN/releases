package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef void (*bodo_ping_result_fn)(void *context, int32_t index, const char *result_json);

// Go can't call a C function pointer, so it calls this.
static void bodo_deliver_ping_result(bodo_ping_result_fn fn, void *context, int32_t index, const char *json) {
	fn(context, index, json);
}
*/
import "C"

import "unsafe"

// deliverPingResult lives apart from the exports: a file with //export may only declare C.
func deliverPingResult(onResult C.bodo_ping_result_fn, context unsafe.Pointer, index int, resultJSON string) {
	text := C.CString(resultJSON)
	defer C.free(unsafe.Pointer(text))
	C.bodo_deliver_ping_result(onResult, context, C.int32_t(index), text)
}
