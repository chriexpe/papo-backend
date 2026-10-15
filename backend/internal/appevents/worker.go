// Package appevents distribui os fatos persistidos sem acoplar o hub humano.
package appevents

import (
	"context"
	"sync"
	"time"

	"papo/internal/storage"
	"papo/internal/utils"
)

// Worker mantém apenas o ciclo de vida; claims e sequências vivem no PostgreSQL.
type Worker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// Start inicia distribuição e poda com operações canceláveis e lotes limitados.
func Start(parent context.Context, retention time.Duration, cap int) *Worker {
	ctx, cancel := context.WithCancel(parent)
	w := &Worker{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		pruneAt := time.Time{}
		for {
			if ctx.Err() != nil {
				return
			}
			op, cancelOp := context.WithTimeout(ctx, 5*time.Second)
			_, err := storage.DispatchAppEvents(op, 50, cap)
			cancelOp()
			if err != nil && ctx.Err() == nil {
				utils.Warnf("eventos de apps: falha no dispatcher: %v", err)
			}
			if time.Now().After(pruneAt) {
				op, cancelOp = context.WithTimeout(ctx, 5*time.Second)
				var pruned int
				pruned, err = storage.PruneAppEvents(op, retention, 200, cap)
				cancelOp()
				if err != nil && ctx.Err() == nil {
					utils.Warnf("eventos de apps: falha na retenção: %v", err)
				}
				pruneAt = time.Now().Add(time.Minute)
				// Enquanto houver lote cheio, continua no próximo tick. Uma poda fixa
				// de 200/min deixaria snapshots crescerem sem limite sob tráfego maior.
				if err == nil && pruned == 200 {
					pruneAt = time.Time{}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
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
