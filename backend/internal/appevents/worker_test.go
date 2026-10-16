package appevents

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestShutdownCanceledWorker verifica cancelamento e encerramentos concorrentes sem depender do banco.
func TestShutdownCanceledWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker := Start(ctx, 0, 0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker.Shutdown() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown bloqueado")
	}
	select {
	case <-worker.done:
	default:
		t.Fatal("worker não encerrou")
	}
}
