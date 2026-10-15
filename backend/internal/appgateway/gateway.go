// Package appgateway implementa o transporte de apps sem usar o hub de usuários humanos.
package appgateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
	"papo/internal/storage"
)

// Frame descreve os controles; somente dispatch inclui o envelope durável do fato.
type Frame struct {
	Op                  string `json:"op"`
	Epoch               int64  `json:"epoch,omitempty"`
	Version             int    `json:"version,omitempty"`
	HeartbeatIntervalMS int64  `json:"heartbeat_interval_ms,omitempty"`
	Cursor              *int64 `json:"cursor,omitempty"`
	ReplayFloor         *int64 `json:"replay_floor,omitempty"`
	LastSeq             *int64 `json:"last_seq,omitempty"`
	*storage.AppEventDelivery
}

// inbound aceita hello, ack e heartbeat com tamanho e frequência limitados.
type inbound struct {
	Op          string `json:"op"`
	ResumeAfter *int64 `json:"resume_after,omitempty"`
	Epoch       int64  `json:"epoch,omitempty"`
	DeliverySeq *int64 `json:"delivery_seq,omitempty"`
}

// outbound mantém somente a sequência até o último instante, para respeitar redações.
type outbound struct {
	frame *Frame
	seq   int64
}

// Registry mantém um consumidor por instalação; a época no banco também cerca outros processos.
type Registry struct {
	mu        sync.Mutex
	clients   map[string]*consumer
	all       map[*consumer]struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	heartbeat time.Duration
	buffer    int
}

// consumer possui um único writer e uma única goroutine leitora, como exige gorilla/websocket.
type consumer struct {
	registry *Registry
	conn     *websocket.Conn
	id       string
	epoch    int64
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	send     chan outbound
	// mu impede que um ack recebido antes de WriteJSON retornar ultrapasse highestSent.
	mu          sync.Mutex
	highestSent int64
	revalidate  func(context.Context) error
}

// NewRegistry cria um registro independente para cada servidor Echo.
func NewRegistry() *Registry {
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{clients: map[string]*consumer{}, all: map[*consumer]struct{}{}, ctx: ctx, cancel: cancel, heartbeat: 30 * time.Second, buffer: 32}
}

// writeFrame limita toda escrita, inclusive o handshake antes da ativação.
func writeFrame(conn *websocket.Conn, frame Frame) error {
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return conn.WriteJSON(frame)
}

// stateFrame expõe o checkpoint de recuperação sem incluir conteúdo.
func stateFrame(op string, s storage.AppGatewayState) Frame {
	return Frame{Op: op, Epoch: s.Epoch, Cursor: &s.Cursor, ReplayFloor: &s.ReplayFloor, LastSeq: &s.LastSeq}
}

// Serve exige autenticação HTTP prévia; revalidate também detecta rotação e revogação no socket.
// O hello do cliente ativa a conexão: upgrades abandonados não substituem consumidores válidos.
func (r *Registry) Serve(w http.ResponseWriter, request *http.Request, id string, revalidate func(context.Context) error) error {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	// A única credencial aceita pelo handler é Bearer explícito, nunca cookies ou query string.
	conn, err := upgrader.Upgrade(w, request, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.ctx)
	c := &consumer{registry: r, conn: conn, id: id, ctx: ctx, cancel: cancel, send: make(chan outbound, r.buffer), revalidate: revalidate}
	defer c.stop()
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return nil
	}
	r.wg.Add(1)
	r.all[c] = struct{}{}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.all, c); r.mu.Unlock() }()
	defer r.wg.Done()
	conn.SetReadLimit(4096)
	if err := writeFrame(conn, Frame{Op: "hello", Version: 1, HeartbeatIntervalMS: r.heartbeat.Milliseconds()}); err != nil {
		return nil
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var hello inbound
	if err := conn.ReadJSON(&hello); err != nil || hello.Op != "hello" {
		return nil
	}
	op, cancelOp := context.WithTimeout(ctx, 5*time.Second)
	if err := revalidate(op); err != nil {
		cancelOp()
		return nil
	}
	// Serializa ativação e troca local. O contexto cancela SQL mesmo durante Shutdown.
	r.mu.Lock()
	if ctx.Err() != nil {
		r.mu.Unlock()
		cancelOp()
		return nil
	}
	state, err := storage.OpenAppGateway(op, id, hello.ResumeAfter)
	if err != nil && !errors.Is(err, storage.ErrAppResyncRequired) {
		r.mu.Unlock()
		cancelOp()
		return nil
	}
	old := r.clients[id]
	c.epoch = state.Epoch
	c.highestSent = state.Cursor
	r.clients[id] = c
	if old != nil {
		old.stop()
	}
	r.mu.Unlock()
	cancelOp()
	defer func() {
		r.mu.Lock()
		if r.clients[id] == c {
			delete(r.clients, id)
		}
		r.mu.Unlock()
	}()
	if errors.Is(err, storage.ErrAppResyncRequired) {
		_ = writeFrame(conn, stateFrame("resync_required", state))
		return nil
	}
	if err := writeFrame(conn, stateFrame("ready", state)); err != nil {
		return nil
	}
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); c.writeLoop() }()
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); c.readLoop() }()
	c.poll(state.Cursor, revalidate)
	c.stop()
	<-writerDone
	<-readerDone
	return nil
}

// stop nunca espera uma escrita de close: fechar o socket interrompe I/O bloqueado.
func (c *consumer) stop() { c.once.Do(func() { c.cancel(); _ = c.conn.Close() }) }

// enqueue desconecta consumidores lentos; o backlog persistido permite reentrega sem perda.
func (c *consumer) enqueue(value outbound) bool {
	select {
	case <-c.ctx.Done():
		return false
	default:
	}
	select {
	case c.send <- value:
		return true
	default:
		c.stop()
		return false
	}
}

// poll busca sequências, nunca snapshots, e revalida a credencial enquanto o socket está aberto.
func (c *consumer) poll(cursor int64, revalidate func(context.Context) error) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		op, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		err := revalidate(op)
		var seqs []int64
		var state storage.AppGatewayState
		if err == nil {
			seqs, state, err = storage.PendingAppGatewaySequences(op, c.id, c.epoch, cursor, 16)
		}
		cancel()
		if errors.Is(err, storage.ErrAppResyncRequired) {
			// O writer envia o controle terminal e encerra, em vez de descartar o frame na fila.
			if c.enqueue(outbound{frame: framePtr(stateFrame("resync_required", state))}) {
				<-c.ctx.Done()
			}
			return
		}
		if err != nil {
			return
		}
		for _, seq := range seqs {
			if !c.enqueue(outbound{seq: seq}) {
				return
			}
			cursor = seq
		}
		select {
		case <-c.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// framePtr permite enfileirar controles sem copiar payloads de eventos.
func framePtr(frame Frame) *Frame { return &frame }

// writeLoop é o único writer depois de ready e relê o snapshot imediatamente antes do envio.
func (c *consumer) writeLoop() {
	defer c.stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case item := <-c.send:
			frame := item.frame
			if frame == nil {
				op, cancel := context.WithTimeout(c.ctx, 5*time.Second)
				var d *storage.AppEventDelivery
				var state storage.AppGatewayState
				err := c.revalidate(op)
				if err == nil {
					d, state, err = storage.ReadAppGatewayDelivery(op, c.id, c.epoch, item.seq)
				}
				cancel()
				if errors.Is(err, storage.ErrAppResyncRequired) {
					_ = writeFrame(c.conn, stateFrame("resync_required", state))
					return
				}
				if err != nil {
					return
				}
				if d == nil {
					continue
				}
				frame = &Frame{Op: "dispatch", Epoch: c.epoch, AppEventDelivery: d}
			}
			c.mu.Lock()
			err := writeFrame(c.conn, *frame)
			if err == nil && frame.AppEventDelivery != nil {
				c.highestSent = frame.DeliverySeq
			}
			c.mu.Unlock()
			if err != nil || frame.Op == "resync_required" {
				return
			}
		}
	}
}

// readLoop impõe heartbeat, limita frames e cerca cada confirmação pela época durável.
func (c *consumer) readLoop() {
	defer c.stop()
	limiter := rate.NewLimiter(100, 200)
	for {
		c.conn.SetReadDeadline(time.Now().Add(3 * c.registry.heartbeat))
		var frame inbound
		if err := c.conn.ReadJSON(&frame); err != nil || !limiter.Allow() {
			return
		}
		switch frame.Op {
		case "heartbeat":
			if frame.Epoch != c.epoch || !c.enqueue(outbound{frame: &Frame{Op: "heartbeat_ack", Epoch: c.epoch}}) {
				return
			}
		case "ack":
			if frame.Epoch != c.epoch || frame.DeliverySeq == nil {
				return
			}
			op, cancel := context.WithTimeout(c.ctx, 5*time.Second)
			c.mu.Lock()
			err := c.revalidate(op)
			if err == nil {
				err = storage.AckAppGateway(op, c.id, c.epoch, *frame.DeliverySeq, c.highestSent)
			}
			c.mu.Unlock()
			cancel()
			if err != nil {
				return
			}
		default:
			return
		}
	}
}

// Shutdown interrompe inclusive hellos incompletos e espera as goroutines com prazo máximo.
func (r *Registry) Shutdown() {
	r.cancel()
	r.mu.Lock()
	for c := range r.all {
		c.stop()
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}
