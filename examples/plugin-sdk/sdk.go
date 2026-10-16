// Package sdk implementa papo-core-v1 para reactors TinyGo, sem Component Model.
package sdk

/*
#include <stdlib.h>
*/
import "C"
import (
	"encoding/json"
	"errors"
	"unsafe"
)

//go:wasmimport papo_host_v1 call
func hostCall(p, n, out, capacity uint32) uint32

// Event é o envelope versionado enviado ao invoke pelo host.
type Event struct {
	Type     string          `json:"type"`
	Settings map[string]any  `json:"settings"`
	Input    json.RawMessage `json:"input"`
}

// Handler trata on_enable, on_disable e on_trigger; estado durável usa KV do host.
var Handler func(Event) (json.RawMessage, error)
var scratch [65536]byte

// pointer converte somente offsets na memória linear do guest.
func pointer(b []byte) uint32 { return uint32(uintptr(unsafe.Pointer(&b[0]))) }

//export alloc
func alloc(n uint32) uint32 { return uint32(uintptr(C.malloc(C.ulong(n)))) }

// Call usa scratch pertencente ao guest e não repete operações após resposta grande.
func Call(req any) (json.RawMessage, error) {
	data, e := json.Marshal(req)
	if e != nil {
		return nil, e
	}
	n := hostCall(pointer(data), uint32(len(data)), pointer(scratch[:]), uint32(len(scratch)))
	if n > uint32(len(scratch)) {
		return nil, errors.New("host response excedida")
	}
	return append([]byte(nil), scratch[:n]...), nil
}

// Asset lê um trecho base64 de arquivo confinado sem expor filesystem WASI.
func Asset(path string, offset, length int64) ([]byte, error) {
	b, e := Call(map[string]any{"method": "asset.read", "path": path, "offset": offset, "length": length})
	if e != nil {
		return nil, e
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			Data []byte `json:"data"`
		} `json:"result"`
	}
	if e = json.Unmarshal(b, &out); e != nil || !out.OK {
		return nil, errors.New("asset recusado")
	}
	return out.Result.Data, nil
}

//export invoke
func invoke(p, n uint32) uint64 {
	data := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(p))), n)
	var ev Event
	var out json.RawMessage
	var err error
	if err = json.Unmarshal(data, &ev); err == nil && Handler != nil {
		out, err = Handler(ev)
	}
	if err != nil {
		out, _ = json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": "guest_error", "detail": err.Error()}})
	}
	if len(out) == 0 {
		out = json.RawMessage(`{"ok":true,"result":null}`)
	}
	ptr := alloc(uint32(len(out)))
	if ptr == 0 {
		return 0
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), len(out)), out)
	return uint64(ptr)<<32 | uint64(len(out))
}
