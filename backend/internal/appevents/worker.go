// Package appevents distribui os fatos persistidos sem acoplar o hub humano.
package appevents

import (
	"context"
	"sync"
	"time"

	"papo/internal/storage"
	"papo/internal/utils"
)

const (
	// dispatchBatch e pruneBatch limitam cada transação; lote cheio faz o
	// worker continuar imediatamente em vez de esperar o próximo sinal.
	dispatchBatch = 50
	pruneBatch    = 200
	// idleInterval é a varredura de segurança sem sinal de escrita (o sinal
	// cobre o caso normal; a varredura cobre falhas inesperadas).
	idleInterval = 10 * time.Second
	// pruneInterval espaça a retenção quando não há lote cheio pendente.
	pruneInterval = time.Minute
)

// Worker mantém apenas o ciclo de vida; claims e sequências vivem no PostgreSQL.
type Worker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// Start inicia distribuição e poda com operações canceláveis e lotes limitados.
// O worker dorme até o commit de uma escrita que produz eventos
// (storage.AppEventsWritten), a varredura de segurança ou a próxima poda.
// onChange, quando não nil, é chamado depois de atribuir ou podar entregas,
// para que os consumidores conectados busquem o novo estado.
func Start(parent context.Context, retention time.Duration, cap int, onChange func()) *Worker {
	ctx, cancel := context.WithCancel(parent)
	w := &Worker{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		idle := time.NewTicker(idleInterval)
		defer idle.Stop()
		pruneAt := time.Time{}
		for ctx.Err() == nil {
			more := false
			changed := false

			op, cancelOp := context.WithTimeout(ctx, 5*time.Second)
			dispatched, err := storage.DispatchAppEvents(op, dispatchBatch, cap)
			cancelOp()
			if err != nil && ctx.Err() == nil {
				utils.Warnf("eventos de apps: falha no dispatcher: %v", err)
			}
			more = more || (err == nil && dispatched == dispatchBatch)
			changed = changed || dispatched > 0

			if time.Now().After(pruneAt) {
				op, cancelOp = context.WithTimeout(ctx, 5*time.Second)
				pruned, err := storage.PruneAppEvents(op, retention, pruneBatch, cap)
				cancelOp()
				if err != nil && ctx.Err() == nil {
					utils.Warnf("eventos de apps: falha na retenção: %v", err)
				}
				pruneAt = time.Now().Add(pruneInterval)
				// Lote cheio continua já: uma poda fixa de 200/min deixaria os
				// snapshots crescerem sem limite sob tráfego maior.
				if err == nil && pruned == pruneBatch {
					pruneAt = time.Time{}
					more = true
				}
				changed = changed || pruned > 0
			}

			if changed && onChange != nil {
				onChange()
			}
			if more {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-storage.AppEventsWritten():
			case <-idle.C:
			case <-time.After(time.Until(pruneAt)):
			}
		}
	}()
	return w
}

// Shutdown cancela SQL em andamento e limita a espera para não travar o servidor.
func (w *Worker) Shutdown() {
	w.once.Do(w.cancel)
	select {
	case <-w.done:
	case <-time.After(6 * time.Second):
		utils.Warnf("eventos de apps: limite de encerramento excedido")
	}
}
