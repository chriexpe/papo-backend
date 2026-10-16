package main

import (
	"encoding/json"
	"errors"
	sdk "papo-plugin-sdk"
)

// init registra o handler durante _initialize, sem persistir estado em globals.
func init() { sdk.Handler = handle }

// handle usa um template externo e posta somente a própria superfície declarada.
func handle(e sdk.Event) (json.RawMessage, error) {
	if e.Type != "on_trigger" {
		return nil, nil
	}
	var input struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(e.Input, &input); err != nil {
		return nil, err
	}
	// O exemplo permite exercitar os host calls pelo trigger administrativo.
	if len(input.Request) > 0 {
		var req any
		if err := json.Unmarshal(input.Request, &req); err != nil {
			return nil, err
		}
		return sdk.Call(req)
	}
	text, err := sdk.Asset("templates/message.txt", 0, 32768)
	if err != nil {
		return nil, err
	}
	channel, ok := e.Settings["target_channel"].(string)
	if !ok || channel == "" {
		return nil, errors.New("target_channel obrigatório")
	}
	return sdk.Call(map[string]any{"method": "message.send", "channel_id": channel, "content": string(text), "embeds": []any{map[string]any{"source_type": "app", "app_surface_id": "result", "title": "Resultado do plugin"}}})
}

// main permanece vazio: este módulo é um reactor WASI.
func main() {}
