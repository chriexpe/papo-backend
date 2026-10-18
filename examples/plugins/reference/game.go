package main

import (
	"encoding/json"
	"errors"

	sdk "papo-plugin-sdk"
)

// game é uma partida de jogo da velha. Cada partida fica na chave game:<id>;
// message:<id> aponta a partida da mensagem (botão Revanche) e player:<user>
// a partida mais recente do jogador, usada por sessões abertas sem convite. A
// fila do host serializa as jogadas da instalação.
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

// get lê key em v; found=false quando a chave não existe.
func get(key string, v any) (bool, error) {
	b, err := sdk.Host(map[string]any{"method": "kv.get", "key": key})
	var hostErr *sdk.HostError
	if errors.As(err, &hostErr) && hostErr.Code == "not_found" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var entry struct {
		Value json.RawMessage `json:"value"`
	}
	if err = json.Unmarshal(b, &entry); err != nil {
		return false, err
	}
	return true, json.Unmarshal(entry.Value, v)
}

func put(key string, v any) error {
	_, err := sdk.Host(map[string]any{"method": "kv.put", "key": key, "value": v})
	return err
}

// loadGame lê a partida apontada por pointer (message:<id> ou player:<user>).
func loadGame(pointer string) (game, error) {
	var id string
	found, err := get(pointer, &id)
	if err != nil {
		return game{}, err
	}
	if !found {
		return game{}, errors.New("partida inexistente")
	}
	return loadGameID(id)
}

func loadGameID(id string) (game, error) {
	var g game
	found, err := get("game:"+id, &g)
	if err == nil && !found {
		err = errors.New("partida inexistente")
	}
	return g, err
}

func saveGame(g game) error { return put("game:"+g.ID, g) }

func gameButtons() [][]sdk.Button {
	return [][]sdk.Button{{
		{ID: "revanche", Label: "Revanche", Style: "primary"},
		{ID: "jogar", Label: "Jogar", Style: "secondary", Opens: "result"},
	}}
}

func publishGame(e sdk.Event, g *game) error {
	req := map[string]any{
		"method":     "message.send",
		"channel_id": e.Settings["target_channel"],
		"content":    "Jogo da velha: abra Jogar ou peça Revanche.",
		"components": gameButtons(),
		"embeds":     []any{map[string]any{"source_type": "app", "app_surface_id": "result", "title": "Jogo da velha"}},
	}
	b, err := sdk.Host(req)
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
	if err = saveGame(*g); err != nil {
		return err
	}
	return put("message:"+msg.ID, g.ID)
}

// inviteGame cria a partida id, convida target e a torna a partida atual dos
// dois jogadores.
func inviteGame(e sdk.Event, user, target, id string) (game, error) {
	g := game{ID: id, Players: [2]string{user, target}}
	invite := sdk.InviteSend{
		Method:   "invite.send",
		ToUserID: target,
		Text:     "Vamos jogar uma partida de jogo da velha?",
		Data:     map[string]any{"game_id": id},
	}
	if _, err := sdk.Host(invite); err != nil {
		return g, err
	}
	if err := publishGame(e, &g); err != nil {
		return g, err
	}
	for _, player := range g.Players {
		if err := put("player:"+player, id); err != nil {
			return g, err
		}
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
		g, err := loadGame("message:" + in.MessageID)
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
		_, err = sdk.Host(map[string]any{
			"method":     "message.edit",
			"message_id": in.MessageID,
			"channel_id": in.ChannelID,
			"content":    "Revanche iniciada!",
			"components": gameButtons(),
		})
		return result(g), err
	}
	return nil, errors.New("interação desconhecida")
}

// sessionGame resolve a partida da sessão: a do convite aceito ou, sem convite,
// a partida atual do jogador.
func sessionGame(in sdk.UICall) (game, error) {
	if in.Invite == nil {
		return loadGame("player:" + in.UserID)
	}
	var data struct {
		GameID string `json:"game_id"`
	}
	if json.Unmarshal(in.Invite.Data, &data) != nil || data.GameID == "" {
		return game{}, errors.New("convite sem partida")
	}
	return loadGameID(data.GameID)
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
	g, err := sessionGame(in)
	if err != nil {
		return nil, err
	}
	if in.UserID != g.Players[0] && in.UserID != g.Players[1] {
		return nil, errors.New("você não participa desta partida")
	}
	switch in.Method {
	case "game.profiles":
		profiles, err := sdk.ResolveUsers(g.Players[:])
		if err != nil {
			return nil, err
		}
		return result(profiles), nil
	case "game.state":
		return result(map[string]any{"game": g, "user_id": in.UserID}), nil
	case "game.move":
		return move(in, g)
	default:
		return nil, errors.New("método desconhecido")
	}
}

// move aplica uma jogada do jogador da vez na revisão atual.
func move(in sdk.UICall, g game) (json.RawMessage, error) {
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
	lines := [][3]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, {0, 3, 6}, {1, 4, 7}, {2, 5, 8}, {0, 4, 8}, {2, 4, 6}}
	for _, line := range lines {
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
	if err := saveGame(g); err != nil {
		return nil, err
	}
	return result(map[string]any{"game": g, "user_id": in.UserID}), nil
}
