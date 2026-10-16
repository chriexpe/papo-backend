// O exemplo encaminha um host request JSON e mantém estado apenas no Postgres.
package main

/*
#include <stdlib.h>
*/
import "C"
import (
	"strings"
	"unsafe"
)

//go:wasmimport papo_host_v1 call
func hostCall(p, n, out, capacity uint32) uint32

var scratch [65536]byte

// pointer retorna um offset na memória linear, nunca um endereço do host.
func pointer(b []byte) uint32 { return uint32(uintptr(unsafe.Pointer(&b[0]))) }

//export alloc
func alloc(n uint32) uint32 { return uint32(uintptr(C.malloc(C.ulong(n)))) }

//export invoke
func invoke(p, n uint32) uint64 {
	request := string(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(p))), n))
	switch {
	case strings.Contains(request, `"op":"trap"`):
		panic("trap")
	case strings.Contains(request, `"op":"loop"`):
		for {
		}
	case strings.Contains(request, `"op":"oversize"`):
		return uint64(131072)
	case strings.Contains(request, `"op":"pointer"`):
		return uint64(0xfffffff0)<<32 | 64
	case strings.Contains(request, `"op":"calls"`):
		for i := 0; i < 33; i++ {
			hostCall(p, n, pointer(scratch[:]), uint32(len(scratch)))
		}
	case strings.Contains(request, `"op":"memory"`):
		_ = make([]byte, 32<<20)
	}
	size := hostCall(p, n, pointer(scratch[:]), uint32(len(scratch)))
	if size > uint32(len(scratch)) {
		return uint64(size)
	}
	out := alloc(size)
	if out == 0 {
		return 0
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(out))), size), scratch[:size])
	return uint64(out)<<32 | uint64(size)
}

// main não inicia um programa: o artefato é um reactor WASI.
func main() {}
