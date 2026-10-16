// BlueMap é um reactor mínimo: o host gerencia sua superfície remota aprovada.
package main

/*
#include <stdlib.h>
*/
import "C"
import "unsafe"

//export alloc
func alloc(n uint32) uint32 { return uint32(uintptr(C.malloc(C.ulong(n)))) }

//export invoke
func invoke(p, n uint32) uint64 {
	data := []byte(`{"ok":true,"result":null}`)
	out := alloc(uint32(len(data)))
	if out == 0 {
		return 0
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(out))), len(data)), data)
	return uint64(out)<<32 | uint64(len(data))
}

// main não inicia processo; _initialize prepara o reactor.
func main() {}
