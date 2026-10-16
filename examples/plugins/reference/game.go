package main

import (
	"encoding/json"
	"errors"
	sdk "papo-plugin-sdk"
)

// game persiste no namespace do plugin; a fila do host serializa as jogadas locais.
type game struct {
	ID        string    `json:"id"`
	Players   [2]string `json:"players"`
	Board     [9]string `json:"board"`
	Turn      int       `json:"turn"`
	Winner    string    `json:"winner"`
	Revision  int       `json:"revision"`
	MessageID string    `json:"message_id"`
}

func value(v any) json.RawMessage  { b, _ := json.Marshal(v); return b }
func result(v any) json.RawMessage { return value(map[string]any{"ok": true, "result": v}) }

// host verifica o envelope: erro de autorização nunca vira sucesso silencioso.
func host(req any) (json.RawMessage, error) {
	b, e := sdk.Call(req)
	if e != nil {
		return nil, e
	}
	var r struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Detail string `json:"detail"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &r) != nil || !r.OK {
		return nil, errors.New("host recusou operação: " + r.Error.Detail)
	}
	return r.Result, nil
}
func loadGame() (game, error) {
	var g game
	b, e := host(map[string]any{"method": "kv.get", "key": "game"})
	if e != nil {
		return g, e
	}
	var entry struct {
		Value game `json:"value"`
	}
	e = json.Unmarshal(b, &entry)
	return entry.Value, e
}
func saveGame(g game) error {
	_, e := host(map[string]any{"method": "kv.put", "key": "game", "value": g})
	return e
}
func gameButtons() [][]sdk.Button {
	return [][]sdk.Button{{{ID: "revanche", Label: "Revanche", Style: "primary"}, {ID: "jogar", Label: "Jogar", Style: "secondary", Opens: "result"}}}
}
func publishGame(e sdk.Event, g *game) error {
	req := map[string]any{"method": "message.send", "channel_id": e.Settings["target_channel"], "content": "Jogo da velha: abra Jogar ou peça Revanche.", "components": gameButtons(), "embeds": []any{map[string]any{"source_type": "app", "app_surface_id": "result", "title": "Jogo da velha"}}}
	b, err := host(req)
	if err != nil {
		return err
	}
	var msg struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(b, &msg); err != nil {
		return err
	}
	g.MessageID = msg.ID
	return saveGame(*g)
}
func inviteGame(e sdk.Event, user, target, id string) (game, error) {
	g := game{ID: id, Players: [2]string{user, target}}
	if _, err := host(sdk.InviteSend{Method: "invite.send", ToUserID: target, Text: "Vamos jogar uma partida de jogo da velha?", Data: map[string]any{"game_id": id}}); err != nil {
		return g, err
	}
	if err := publishGame(e, &g); err != nil {
		return g, err
	}
	return g, nil
}
func interaction(e sdk.Event) (json.RawMessage, error) {
	var in sdk.Interaction
	if err := json.Unmarshal(e.Input, &in); err != nil {
		return nil, err
	}
	if in.Kind == "user_action" && in.ActionID == "convidar" {
		g, err := inviteGame(e, in.UserID, in.TargetUserID, in.InteractionID)
		return result(g), err
	}
	if in.Kind == "button" && in.ComponentID == "revanche" {
		g, err := loadGame()
		if err != nil {
			return nil, err
		}
		if in.UserID != g.Players[0] && in.UserID != g.Players[1] {
			return nil, errors.New("somente os jogadores podem pedir revanche")
		}
		g.Board = [9]string{}
		g.Turn = 0
		g.Winner = ""
		g.Revision++
		if err = saveGame(g); err != nil {
			return nil, err
		}
		_, err = host(map[string]any{"method": "message.edit", "message_id": in.MessageID, "channel_id": in.ChannelID, "content": "Revanche iniciada!", "components": gameButtons()})
		return result(g), err
	}
	return nil, errors.New("interação desconhecida")
}
func uiCall(e sdk.Event) (json.RawMessage, error) {
	var in sdk.UICall
	if err := json.Unmarshal(e.Input, &in); err != nil {
		return nil, err
	}
	if in.Method == "game.invite" {
		var p struct {
			Target string `json:"target_user_id"`
		}
		if err := json.Unmarshal(in.Params, &p); err != nil {
			return nil, err
		}
		g, err := inviteGame(e, in.UserID, p.Target, in.SessionID)
		return result(g), err
	}
	if in.Method == "identity" {
		return result(map[string]any{"user_id": in.UserID, "user": in.User, "channel_id": in.ChannelID, "session_id": in.SessionID}), nil
	}
	g, err := loadGame()
	if err != nil {
		return nil, err
	}
	if in.UserID != g.Players[0] && in.UserID != g.Players[1] {
		return nil, errors.New("você não participa desta partida")
	}
	if in.Invite != nil {
		var data struct {
			GameID string `json:"game_id"`
		}
		if json.Unmarshal(in.Invite.Data, &data) != nil || data.GameID != g.ID {
			return nil, errors.New("convite de outra partida")
		}
	}
	switch in.Method {
	case "game.profiles":
		profiles, e := sdk.ResolveUsers(g.Players[:])
		if e != nil {
			return nil, e
		}
		return result(profiles), nil
	case "game.state":
		return result(map[string]any{"game": g, "user_id": in.UserID}), nil
	case "game.move":
		var p struct {
			Cell     int `json:"cell"`
			Revision int `json:"revision"`
		}
		if json.Unmarshal(in.Params, &p) != nil || p.Cell < 0 || p.Cell > 8 {
			return nil, errors.New("casa inválida")
		}
		if g.Winner != "" || g.Players[g.Turn] != in.UserID || g.Board[p.Cell] != "" || p.Revision != g.Revision {
			return nil, errors.New("jogada ou revisão inválida")
		}
		mark := "X"
		if g.Turn == 1 {
			mark = "O"
		}
		g.Board[p.Cell] = mark
		g.Turn = 1 - g.Turn
		g.Revision++
		for _, line := range [][3]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, {0, 3, 6}, {1, 4, 7}, {2, 5, 8}, {0, 4, 8}, {2, 4, 6}} {
			if g.Board[line[0]] != "" && g.Board[line[0]] == g.Board[line[1]] && g.Board[line[1]] == g.Board[line[2]] {
				g.Winner = mark
			}
		}
		full := true
		for _, cell := range g.Board {
			if cell == "" {
				full = false
			}
		}
		if full && g.Winner == "" {
			g.Winner = "empate"
		}
		if err = saveGame(g); err != nil {
			return nil, err
		}
		return result(map[string]any{"game": g, "user_id": in.UserID}), nil
	default:
		return nil, errors.New("método desconhecido")
	}
}
