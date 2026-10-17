package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"papo/internal/models"
	"papo/internal/storage"
)

// gatewayFrame lê o contrato público sem depender das estruturas internas do registry.
type gatewayFrame struct {
	Op          string         `json:"op"`
	Epoch       int64          `json:"epoch"`
	Cursor      int64          `json:"cursor"`
	ReplayFloor int64          `json:"replay_floor"`
	LastSeq     int64          `json:"last_seq"`
	DeliverySeq int64          `json:"delivery_seq"`
	EventID     string         `json:"event_id"`
	Type        string         `json:"type"`
	Payload     map[string]any `json:"payload"`
}

// gatewayFixture usa newApp e HTTP real para incluir autenticação e o upgrade.
func gatewayFixture(t *testing.T) (*httptest.Server, models.AppInstallation, string, models.Channel) {
	t.Helper()
	server, _, _, app, token, channel := gatewayAdminFixture(t)
	return server, app, token, channel
}

// gatewayAdminFixture também devolve o roteador e o cookie do administrador,
// para exercitar as rotas de gestão contra o mesmo registry do gateway.
func gatewayAdminFixture(t *testing.T) (*httptest.Server, *echo.Echo, string, models.AppInstallation, string, models.Channel) {
	t.Helper()
	cleanServers(context.Background())
	e := newApp()
	cookie := webhookTestServer(t, e)
	app, token := createAppViaRoute(t, e, cookie, "remote")
	channel := createChannelFor(t, "gateway_"+randHex(4))
	scopeAppViaRoute(t, e, cookie, app.ID, channel.ID)
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)
	return server, e, cookie, app, token, channel
}

// gatewayExpectClosed exige que o servidor encerre o socket; esgotar o prazo
// de leitura não conta como encerramento.
func gatewayExpectClosed(t *testing.T, conn *gorilla.Conn, reason string) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var frame gatewayFrame
	err := conn.ReadJSON(&frame)
	var netErr net.Error
	if err == nil || (errors.As(err, &netErr) && netErr.Timeout()) {
		t.Fatalf("%s não encerrou o socket: %v", reason, err)
	}
}

// gatewayUpgrade autentica exclusivamente por header, lê hello e deixa a ativação para o teste.
func gatewayUpgrade(t *testing.T, server *httptest.Server, token string) *gorilla.Conn {
	t.Helper()
	conn, response, err := gorilla.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/app/gateway", http.Header{"Authorization": []string{"Bearer " + token}})
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hello gatewayFrame
	if err := conn.ReadJSON(&hello); err != nil || hello.Op != "hello" {
		t.Fatalf("hello=%+v %v", hello, err)
	}
	return conn
}

// gatewayActivate declara um checkpoint opcional e retorna ready ou resync_required.
func gatewayActivate(conn *gorilla.Conn, resume *int64) (gatewayFrame, error) {
	hello := map[string]any{"op": "hello"}
	if resume != nil {
		hello["resume_after"] = *resume
	}
	var ready gatewayFrame
	if err := conn.WriteJSON(hello); err != nil {
		return ready, err
	}
	err := conn.ReadJSON(&ready)
	return ready, err
}

// gatewayConnect exige a sequência hello -> ready antes de consumir os eventos.
func gatewayConnect(t *testing.T, server *httptest.Server, token string, resume *int64) (*gorilla.Conn, gatewayFrame) {
	t.Helper()
	conn := gatewayUpgrade(t, server, token)
	ready, err := gatewayActivate(conn, resume)
	if err != nil || ready.Op != "ready" || ready.Epoch <= 0 {
		t.Fatalf("ready=%+v %v", ready, err)
	}
	return conn, ready
}

// gatewayRead limita a espera para impedir testes bloqueados por regressões no transporte.
func gatewayRead(t *testing.T, conn *gorilla.Conn) gatewayFrame {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var frame gatewayFrame
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

// gatewayMessage produz um fato confirmado e atribuído para o leitor real.
func gatewayMessage(t *testing.T, app models.AppInstallation, ch models.Channel, content string) models.Message {
	t.Helper()
	m, err := storage.CreateMessage(context.Background(), ch.ID, app.CreatedBy, content, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchAndWake(); err != nil {
		t.Fatal(err)
	}
	return m
}

// dispatchAndWake distribui o outbox e acorda os consumidores conectados,
// como o worker de eventos faz em produção.
func dispatchAndWake() (int, error) {
	n, err := storage.DispatchAppEvents(context.Background(), 1000, 10000)
	testAppGateway.Wake()
	return n, err
}

// pruneAndWake expira todo o outbox e acorda os consumidores conectados.
func pruneAndWake() (int, error) {
	n, err := storage.PruneAppEvents(context.Background(), 0, 1000, 10000)
	testAppGateway.Wake()
	return n, err
}

// gatewayAck confirma pelo socket e espera o estado durável, sem presumir o escalonamento do reader.
func gatewayAck(t *testing.T, conn *gorilla.Conn, appID string, epoch, seq int64) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"op": "ack", "epoch": epoch, "delivery_seq": seq}); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		state, err := storage.GetAppGatewayState(context.Background(), appID, epoch)
		if err != nil {
			t.Fatal(err)
		}
		if state.AckedSeq >= seq {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("ack não persistiu")
		case <-poll.C:
		}
	}
}

// TestAppGatewayReplayWithoutAck simula crash após envio, reentrega estável e avanço após confirmação.
func TestAppGatewayReplayWithoutAck(t *testing.T) {
	server, app, token, ch := gatewayFixture(t)
	first, ready := gatewayConnect(t, server, token, nil)
	gatewayMessage(t, app, ch, "live")
	sent := gatewayRead(t, first)
	if sent.Op != "dispatch" || sent.Type != "message.created" || sent.DeliverySeq != 1 {
		t.Fatalf("dispatch=%+v", sent)
	}
	first.Close() // Sem ack: o processo consumidor poderia ter morrido aqui.
	// Um novo servidor/registry descarta também todo o estado efêmero do transporte.
	restarted := httptest.NewServer(newApp())
	t.Cleanup(restarted.Close)
	second, ready2 := gatewayConnect(t, restarted, token, nil)
	replay := gatewayRead(t, second)
	if replay.EventID != sent.EventID || replay.DeliverySeq != sent.DeliverySeq || ready2.Epoch <= ready.Epoch {
		t.Fatalf("replay=%+v ready=%+v", replay, ready2)
	}
	dedupe := map[string]bool{}
	effects := 0
	for _, event := range []gatewayFrame{sent, replay} {
		if !dedupe[event.EventID] {
			dedupe[event.EventID] = true
			effects++
		}
	}
	if effects != 1 {
		t.Fatal("event_id não deduplicou a reentrega")
	}
	gatewayAck(t, second, app.ID, ready2.Epoch, replay.DeliverySeq)
	second.Close()
	third, ready3 := gatewayConnect(t, restarted, token, nil)
	if ready3.Cursor != 1 {
		t.Fatalf("checkpoint=%+v", ready3)
	}
	gatewayMessage(t, app, ch, "after ack")
	next := gatewayRead(t, third)
	if next.DeliverySeq != 2 || next.EventID == sent.EventID {
		t.Fatalf("evento confirmado reapareceu: %+v", next)
	}
}

// TestAppGatewayOfflineRedaction entrega created(redacted), updated(redacted), deleted nessa ordem.
func TestAppGatewayOfflineRedaction(t *testing.T) {
	server, app, token, ch := gatewayFixture(t)
	first, _ := gatewayConnect(t, server, token, nil)
	first.Close()
	m := gatewayMessage(t, app, ch, "hello")
	edit := "edit"
	if _, err := storage.UpdateMessage(context.Background(), m.ID, models.Message{Content: &edit}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteMessage(context.Background(), m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchAndWake(); err != nil {
		t.Fatal(err)
	}
	conn, ready := gatewayConnect(t, server, token, nil)
	for i, kind := range []string{"message.created", "message.updated", "message.deleted"} {
		frame := gatewayRead(t, conn)
		if frame.Type != kind || frame.DeliverySeq != int64(i+1) {
			t.Fatalf("ordem=%+v", frame)
		}
		if i < 2 && (len(frame.Payload) != 3 || frame.Payload["redacted"] != true) {
			t.Fatalf("vazamento offline=%v", frame.Payload)
		}
	}
	gatewayAck(t, conn, app.ID, ready.Epoch, 3)
}

// TestAppGatewayConcurrentConnections cerca a conexão perdedora mesmo quando os hellos disputam a época.
func TestAppGatewayConcurrentConnections(t *testing.T) {
	server, app, token, ch := gatewayFixture(t)
	conns := []*gorilla.Conn{gatewayUpgrade(t, server, token), gatewayUpgrade(t, server, token)}
	type result struct {
		index int
		ready gatewayFrame
		err   error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i, conn := range conns {
		wg.Add(1)
		go func(i int, conn *gorilla.Conn) {
			defer wg.Done()
			ready, err := gatewayActivate(conn, nil)
			results <- result{i, ready, err}
		}(i, conn)
	}
	wg.Wait()
	close(results)
	winner := -1
	var ready gatewayFrame
	for res := range results {
		if res.err == nil && res.ready.Op == "ready" && res.ready.Epoch > ready.Epoch {
			winner = res.index
			ready = res.ready
		}
	}
	if winner < 0 || ready.Epoch != 2 {
		t.Fatalf("nenhum vencedor com época 2: %+v", ready)
	}
	if err := storage.AckAppGateway(context.Background(), app.ID, 1, 0, 0); !errors.Is(err, storage.ErrAppGatewayFenced) {
		t.Fatalf("época antiga confirmou: %v", err)
	}
	gatewayMessage(t, app, ch, "winner")
	frame := gatewayRead(t, conns[winner])
	if frame.Epoch != 2 {
		t.Fatalf("epoch=%+v", frame)
	}
	gatewayAck(t, conns[winner], app.ID, 2, frame.DeliverySeq)
	loser := conns[1-winner]
	loser.SetReadDeadline(time.Now().Add(time.Second))
	var old gatewayFrame
	// Um ready já escrito pode estar no socket; a conexão seguinte precisa terminar.
	if err := loser.ReadJSON(&old); err == nil {
		if err := loser.ReadJSON(&old); err == nil {
			t.Fatal("conexão antiga permaneceu ativa")
		}
	}
}

// TestAppGatewayTombstonesWithoutScope respeita a exceção de tombstones após perder escopo e leitura.
func TestAppGatewayTombstonesWithoutScope(t *testing.T) {
	server, app, token, ch := gatewayFixture(t)
	m := gatewayMessage(t, app, ch, "do not deliver")
	if err := storage.DeleteMessage(context.Background(), m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchAndWake(); err != nil {
		t.Fatal(err)
	}
	if err := storage.RemoveAppScope(context.Background(), app.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.UpdateApp(context.Background(), app.ID, nil, nil, nil, &models.AppCapabilities{}); err != nil {
		t.Fatal(err)
	}
	conn, ready := gatewayConnect(t, server, token, nil)
	frame := gatewayRead(t, conn)
	if frame.Type != "message.deleted" || len(frame.Payload) != 2 {
		t.Fatalf("sem escopo=%+v", frame)
	}
	gatewayAck(t, conn, app.ID, ready.Epoch, frame.DeliverySeq)
}

// TestAppGatewayChannelDeletedOffline entrega o destinatário capturado antes da cascata dos escopos.
func TestAppGatewayChannelDeletedOffline(t *testing.T) {
	server, app, token, ch := gatewayFixture(t)
	gatewayMessage(t, app, ch, "deleted channel")
	if err := storage.DeleteChannel(context.Background(), ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchAndWake(); err != nil {
		t.Fatal(err)
	}
	conn, _ := gatewayConnect(t, server, token, nil)
	frame := gatewayRead(t, conn)
	if frame.Type != "channel.deleted" || len(frame.Payload) != 1 || frame.Payload["channel_id"] != ch.ID {
		t.Fatalf("channel.deleted=%+v", frame)
	}
}

// TestAppGatewayRetentionResync informa o piso, permite reconciliação REST e aceita um checkpoint recuperado.
func TestAppGatewayRetentionResync(t *testing.T) {
	server, app, token, ch := gatewayFixture(t)
	gatewayMessage(t, app, ch, "expired")
	if _, err := storage.GetDB().Exec("UPDATE app_event_outbox SET created_at=clock_timestamp()-interval '8 days' WHERE channel_id=$1", ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pruneAndWake(); err != nil {
		t.Fatal(err)
	}
	conn := gatewayUpgrade(t, server, token)
	zero := int64(0)
	frame, err := gatewayActivate(conn, &zero)
	if err != nil || frame.Op != "resync_required" || frame.ReplayFloor != 1 || frame.LastSeq != 1 {
		t.Fatalf("resync=%+v %v", frame, err)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/app/channels/"+ch.ID+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("reconciliação REST=%d", response.StatusCode)
	}
	recovered, ready := gatewayConnect(t, server, token, &frame.LastSeq)
	if ready.Cursor != 1 {
		t.Fatalf("recuperação=%+v", ready)
	}
	gatewayMessage(t, app, ch, "after resync")
	if next := gatewayRead(t, recovered); next.DeliverySeq != 2 {
		t.Fatalf("pós-resync=%+v", next)
	}
}

// TestAppGatewayBearerFailures mantém o mesmo 401 para cookie, query, segredo errado, rotacionado e revogado.
func TestAppGatewayBearerFailures(t *testing.T) {
	server, app, token, _ := gatewayFixture(t)
	newToken := strings.Repeat("a", 64)
	sum := sha256.Sum256([]byte(newToken))
	if err := storage.RotateAppToken(context.Background(), app.ID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		header http.Header
		query  string
	}{
		{"missing", nil, ""}, {"wrong", http.Header{"Authorization": []string{"Bearer wrong"}}, ""},
		{"rotated", http.Header{"Authorization": []string{"Bearer " + token}}, ""},
		{"cookie", http.Header{"Cookie": []string{"Auth=" + newToken}}, ""},
		{"query", nil, "?token=" + newToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, response, err := gorilla.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/app/gateway"+tc.query, tc.header)
			if conn != nil {
				conn.Close()
			}
			if response != nil {
				defer response.Body.Close()
			}
			if err == nil || response == nil || response.StatusCode != 401 {
				t.Fatalf("auth: response=%+v err=%v", response, err)
			}
			var problem map[string]any
			if err := json.NewDecoder(response.Body).Decode(&problem); err != nil || problem["detail"] != "app inválido" {
				t.Fatalf("problema=%v %v", problem, err)
			}
		})
	}

	conn, _ := gatewayConnect(t, server, newToken, nil)
	thirdToken := strings.Repeat("b", 64)
	thirdSum := sha256.Sum256([]byte(thirdToken))
	if err := storage.RotateAppToken(context.Background(), app.ID, hex.EncodeToString(thirdSum[:])); err != nil {
		t.Fatal(err)
	}
	// Rotação fora das rotas de gestão é detectada pelo reexame periódico; o
	// teste força o mesmo efeito do Disconnect que a rota faz.
	testAppGateway.Disconnect(app.ID)
	gatewayExpectClosed(t, conn, "rotação")
	if err := storage.RevokeApp(context.Background(), app.ID); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/app/gateway", nil)
	request.Header.Set("Authorization", "Bearer "+thirdToken)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("revogado=%d", response.StatusCode)
	}

}

// TestAppGatewayManagementDisconnects garante que rotacionar e revogar pelas
// rotas de gestão encerram na hora o socket aberto com a credencial anterior.
func TestAppGatewayManagementDisconnects(t *testing.T) {
	server, e, cookie, app, token, _ := gatewayAdminFixture(t)
	conn, _ := gatewayConnect(t, server, token, nil)
	rec := do(t, e, http.MethodPost, "/apps/"+app.ID+"/token", nil, authCookie(cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("rotação=%d %s", rec.Code, rec.Body.String())
	}
	var rotated struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil || rotated.Token == "" {
		t.Fatalf("token rotacionado: %v", err)
	}
	gatewayExpectClosed(t, conn, "rotação pela rota")

	revoked, _ := gatewayConnect(t, server, rotated.Token, nil)
	if rec := do(t, e, http.MethodDelete, "/apps/"+app.ID, nil, authCookie(cookie)); rec.Code != http.StatusNoContent {
		t.Fatalf("revogação=%d %s", rec.Code, rec.Body.String())
	}
	gatewayExpectClosed(t, revoked, "revogação pela rota")
}

// TestAppGatewayRejectsFutureAck impede que um consumidor confirme fatos ainda não enviados.
func TestAppGatewayRejectsFutureAck(t *testing.T) {
	server, app, token, _ := gatewayFixture(t)
	conn, ready := gatewayConnect(t, server, token, nil)
	if err := conn.WriteJSON(map[string]any{"op": "ack", "epoch": ready.Epoch, "delivery_seq": 100}); err != nil {
		t.Fatal(err)
	}
	var frame gatewayFrame
	if err := conn.ReadJSON(&frame); err == nil {
		t.Fatal("ack futuro não encerrou socket")
	}
	state, err := storage.GetAppGatewayState(context.Background(), app.ID, ready.Epoch)
	if err != nil || state.AckedSeq != 0 {
		t.Fatalf("ack futuro alterou estado: %+v %v", state, err)
	}
}

// TestAppGatewayScopeAndPrivateChannel filtra canais fora do escopo e DMs, preservando a ACL de máquina em canal privado.
func TestAppGatewayScopeAndPrivateChannel(t *testing.T) {
	server, app, token, private := gatewayFixture(t)
	if _, err := storage.GetDB().Exec(`UPDATE channels SET permissions='{"private-role":{"read_channel":false}}' WHERE id=$1`, private.ID); err != nil {
		t.Fatal(err)
	}
	unscoped := createChannelFor(t, "unscoped_gateway_"+randHex(4))
	dm := createChannelFor(t, "dm_gateway_"+randHex(4))
	if _, err := storage.GetDB().Exec("UPDATE channels SET type='dm' WHERE id=$1", dm.ID); err != nil {
		t.Fatal(err)
	}
	// Mesmo um escopo inválido inserido diretamente não habilita eventos de DM.
	if err := storage.AddAppScope(context.Background(), app.ID, dm.ID, app.CreatedBy); err != nil {
		t.Fatal(err)
	}
	gatewayMessage(t, app, unscoped, "hidden")
	gatewayMessage(t, app, dm, "dm hidden")
	allowed := gatewayMessage(t, app, private, "private scoped")
	conn, _ := gatewayConnect(t, server, token, nil)
	frame := gatewayRead(t, conn)
	if frame.Type != "message.created" || frame.Payload["message_id"] != allowed.ID || frame.DeliverySeq != 1 {
		t.Fatalf("isolamento=%+v", frame)
	}
	gatewayMessage(t, app, private, "next allowed")
	if next := gatewayRead(t, conn); next.DeliverySeq != 2 {
		t.Fatalf("canal indevido vazou: %+v", next)
	}
}

// TestAppGatewayReactionEvents mantém o contrato necessário ao leitor Wormhole.
func TestAppGatewayReactionEvents(t *testing.T) {
	server, app, token, ch := gatewayFixture(t)
	conn, _ := gatewayConnect(t, server, token, nil)
	m := gatewayMessage(t, app, ch, "react")
	gatewayRead(t, conn)
	emoji := "👍"
	if _, _, _, err := storage.AddReaction(context.Background(), m.ID, app.CreatedBy, nil, &emoji); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.RemoveReaction(context.Background(), m.ID, app.CreatedBy, nil, &emoji); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchAndWake(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"reaction.added", "reaction.removed"} {
		frame := gatewayRead(t, conn)
		if frame.Type != kind || frame.Payload["unicode"] != emoji || frame.Payload["user_id"] != app.CreatedBy || frame.Payload["message_id"] != m.ID {
			t.Fatalf("reação=%+v", frame)
		}
	}
}
