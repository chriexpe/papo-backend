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

// Handler trata lifecycle, trigger, on_interaction e on_ui_call; estado durável usa KV do host.
var Handler func(Event) (json.RawMessage, error)
var scratch [65536]byte

// pointer converte somente offsets na memória linear do guest.
func pointer(b []byte) uint32 { return uint32(uintptr(unsafe.Pointer(&b[0]))) }

//export alloc
func alloc(n uint32) uint32 { return uint32(uintptr(C.malloc(C.ulong(n)))) }

// Call envia uma requisição ao host e devolve o envelope bruto
// {"ok":...,"result":...,"error":{"code":...}}. Usa scratch pertencente ao
// guest e não repete operações após resposta grande.
func Call(req any) (json.RawMessage, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	n := hostCall(pointer(data), uint32(len(data)), pointer(scratch[:]), uint32(len(scratch)))
	if n > uint32(len(scratch)) {
		return nil, errors.New("resposta do host excedida")
	}
	return append([]byte(nil), scratch[:n]...), nil
}

// HostError é uma recusa do host. Code é forbidden, invalid_param, not_found,
// rate_limited, quota_exceeded ou internal; o host não envia detalhes.
type HostError struct {
	Code string
}

func (e *HostError) Error() string { return "host recusou a operação: " + e.Code }

// Host envia req e devolve somente o result; uma recusa vira *HostError.
func Host(req any) (json.RawMessage, error) {
	b, err := Call(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, &HostError{Code: out.Error.Code}
	}
	return out.Result, nil
}

// Asset lê um trecho base64 de arquivo confinado sem expor filesystem WASI.
func Asset(path string, offset, length int64) ([]byte, error) {
	b, err := Host(map[string]any{"method": "asset.read", "path": path, "offset": offset, "length": length})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []byte `json:"data"`
	}
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
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

// Interaction contém somente identidades verificadas pelo servidor.
type Interaction struct {
	User          UserProfile  `json:"user"`
	TargetUser    *UserProfile `json:"target_user,omitempty"`
	InteractionID string       `json:"interaction_id"`
	Kind          string       `json:"kind"`
	UserID        string       `json:"user_id"`
	ChannelID     string       `json:"channel_id"`
	MessageID     string       `json:"message_id,omitempty"`
	ComponentID   string       `json:"component_id,omitempty"`
	ActionID      string       `json:"action_id,omitempty"`
	TargetUserID  string       `json:"target_user_id,omitempty"`
}

// Invite é o contexto autorizado após aceitação do destinatário.
type Invite struct {
	ID        string          `json:"id"`
	InviterID string          `json:"inviter_id"`
	Data      json.RawMessage `json:"data"`
}

// UICall nunca aceita identidade fornecida pela página.
type UICall struct {
	User      UserProfile     `json:"user"`
	UserID    string          `json:"user_id"`
	ChannelID string          `json:"channel_id"`
	SessionID string          `json:"session_id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	Invite    *Invite         `json:"invite,omitempty"`
}

// InviteSend é permitido somente em on_interaction ou on_ui_call humanos.
type InviteSend struct {
	Method   string `json:"method"`
	ToUserID string `json:"to_user_id"`
	Text     string `json:"text"`
	Data     any    `json:"data"`
}

// Button é um controle nativo em uma das duas linhas de components.
type Button struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Style    string `json:"style"`
	Disabled bool   `json:"disabled"`
	Opens    string `json:"opens,omitempty"`
}

// UserProfile é o único perfil público disponível ao plugin.
type UserProfile struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	// AvatarURL é nil quando o usuário não tem avatar.
	AvatarURL *string `json:"avatar_url"`
}

// ResolveUsers retorna somente perfis autorizados; ausentes e negados são omitidos.
func ResolveUsers(ids []string) ([]UserProfile, error) {
	b, err := Host(map[string]any{"method": "users.resolve", "ids": ids})
	if err != nil {
		return nil, err
	}
	var out []UserProfile
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
