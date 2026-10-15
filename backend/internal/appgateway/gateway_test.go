package appgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// socketPair fornece um socket real para testar I/O e cancelamento sem mocks de WebSocket.
func socketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	received := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			received <- conn
		}
	}))
	t.Cleanup(server.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	serverConn := <-received
	t.Cleanup(func() { client.Close(); serverConn.Close() })
	return client, serverConn
}

// TestSlowConsumerDisconnects verifica o limite da fila com um writer que não progride.
func TestSlowConsumerDisconnects(t *testing.T) {
	client, server := socketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &consumer{conn: server, ctx: ctx, cancel: cancel, send: make(chan outbound, 1)}
	if !c.enqueue(outbound{seq: 1}) {
		t.Fatal("primeiro item recusado")
	}
	if c.enqueue(outbound{seq: 2}) {
		t.Fatal("fila excedeu o limite")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("consumidor lento não foi encerrado")
	}
	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("socket permaneceu aberto")
	}
}

// TestHeartbeatTimeout exige atividade periódica e confirma heartbeat na época correta.
func TestHeartbeatTimeout(t *testing.T) {
	client, server := socketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := NewRegistry()
	registry.heartbeat = 20 * time.Millisecond
	defer registry.Shutdown()
	c := &consumer{registry: registry, conn: server, ctx: ctx, cancel: cancel, epoch: 7, send: make(chan outbound, 2)}
	done := make(chan struct{})
	go func() { defer close(done); c.readLoop() }()
	if err := client.WriteJSON(map[string]any{"op": "heartbeat", "epoch": 7}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-c.send:
		if msg.frame == nil || msg.frame.Op != "heartbeat_ack" {
			t.Fatalf("resposta=%+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat sem resposta")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat expirado não encerrou conexão")
	}
}

// TestShutdownClosesIncompleteHello cobre sockets promovidos que nunca ativaram uma instalação.
func TestShutdownClosesIncompleteHello(t *testing.T) {
	registry := NewRegistry()
	defer registry.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = registry.Serve(w, r, "unused", func(context.Context) error { return nil })
	}))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var hello Frame
	if err := client.ReadJSON(&hello); err != nil || hello.Op != "hello" {
		t.Fatalf("hello=%+v %v", hello, err)
	}
	start := time.Now()
	registry.Shutdown()
	if time.Since(start) > time.Second {
		t.Fatal("shutdown aguardou o prazo de hello")
	}
	client.SetReadDeadline(time.Now().Add(time.Second))
	if err := client.ReadJSON(&hello); err == nil {
		t.Fatal("socket incompleto permaneceu aberto")
	}
}
