//go:build wasip1

// Command policywasm is the Wayglass policy evaluator as a WASI reactor
// module for the Worker (docs/ARCHITECTURE.md D3). It exports one pure
// function and does no I/O, so the Worker decides with exactly the code the
// CLI previews with.
//
// Build: make wasm (GOOS=wasip1 GOARCH=wasm, -buildmode=c-shared).
//
// Calling convention, all offsets into the module's linear memory:
//
//	ptr := wg_alloc(n)              // Go-owned buffer for an n-byte request
//	(copy the request JSON to ptr)
//	r := wg_evaluate(n)             // evaluates the buffer, returns ptr<<32 | len
//	(read the response JSON at r>>32, length r&0xffffffff)
//
// Both buffers stay valid until the next wg_alloc call. The module is
// single-threaded, like the isolate that hosts it.
package main

import (
	"unsafe"

	"github.com/wayglass/wayglass/internal/policy"
)

// Globals keep the buffers reachable, so Go's non-moving GC leaves them
// where the host is reading and writing.
var in, out []byte

// offset returns the linear-memory address of b's first byte.
func offset(b []byte) uint32 {
	return uint32(uintptr(unsafe.Pointer(&b[0]))) //nolint:gosec // the host needs the address; wasm32 pointers fit in 32 bits
}

//go:wasmexport wg_alloc
func wgAlloc(n uint32) uint32 {
	in = make([]byte, max(n, 1))
	out = nil
	return offset(in)
}

//go:wasmexport wg_evaluate
func wgEvaluate(n uint32) uint64 {
	out = policy.EvaluateJSON(in[:min(int(n), len(in))])
	if len(out) == 0 {
		return 0
	}
	return uint64(offset(out))<<32 | uint64(len(out))
}

//go:wasmexport wg_wire_version
func wgWireVersion() uint32 { return policy.WireVersion }

// main is not run in a c-shared (reactor) build; the host calls _initialize.
func main() {}
