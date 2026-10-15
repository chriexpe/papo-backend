package wasmhost

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"papo/internal/plugins"
)

// fixture lê bytes reais gerados pelo TinyGo, sem substituir o runtime sob teste.
func fixture(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/adversarial.wasm")
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// newHost inicia e fecha um runtime real com limites do teste.
func newHost(t *testing.T, l Limits) *Host {
	t.Helper()
	h, e := New(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close(context.Background()) })
	return h
}

// echoCall permite exercitar o transporte binário sem banco.
func echoCall(_ context.Context, b []byte) ([]byte, error) { return b, nil }

// TestBoundaries confirma trap, loop, saída e offsets inválidos sem contaminar outra instalação.
func TestBoundaries(t *testing.T) {
	l := DefaultLimits()
	l.Deadline = 150 * time.Millisecond
	h := newHost(t, l)
	b := fixture(t)
	if e := h.Validate(context.Background(), b); e != nil {
		t.Fatal(e)
	}
	for _, op := range []string{"trap", "loop", "oversize", "pointer", "calls", "memory"} {
		t.Run(op, func(t *testing.T) {
			_, err := h.Invoke(context.Background(), op, b, []byte(`{"op":"`+op+`"}`), echoCall)
			if err == nil {
				t.Fatal("guest malicioso aceito")
			}
			out, err := h.Invoke(context.Background(), "healthy", b, []byte(`{"ok":true}`), echoCall)
			if err != nil || string(out) != `{"ok":true}` {
				t.Fatalf("host afetado: %s %v", out, err)
			}
		})
	}
}

// TestQuarantine exige falhas consecutivas e permite recuperação explícita por upload.
func TestQuarantine(t *testing.T) {
	h := newHost(t, DefaultLimits())
	b := fixture(t)
	for i := 0; i < 3; i++ {
		if _, e := h.Invoke(context.Background(), "bad", b, []byte(`{"op":"trap"}`), echoCall); e == nil {
			t.Fatal("sem trap")
		}
	}
	if _, e := h.Invoke(context.Background(), "bad", b, []byte(`{"ok":true}`), echoCall); !errors.Is(e, plugins.ErrQuarantined) {
		t.Fatal(e)
	}
	h.Reset("bad")
	if _, e := h.Invoke(context.Background(), "bad", b, []byte(`{"ok":true}`), echoCall); e != nil {
		t.Fatal(e)
	}
}

// TestAdmission comprova fila limitada, serialização por instalação e concorrência global.
func TestAdmission(t *testing.T) {
	l := DefaultLimits()
	l.Concurrency = 1
	l.Queue = 1
	l.Deadline = time.Second
	h := newHost(t, l)
	b := fixture(t)
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	done := make(chan error, 3)
	var active atomic.Int32
	call := func(ctx context.Context, b []byte) ([]byte, error) {
		if active.Add(1) != 1 {
			t.Error("concorrência excedida")
		}
		defer active.Add(-1)
		entered <- struct{}{}
		select {
		case <-release:
			return b, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	go func() { _, e := h.Invoke(context.Background(), "one", b, []byte(`{"ok":true}`), call); done <- e }()
	<-entered
	go func() { _, e := h.Invoke(context.Background(), "one", b, []byte(`{"ok":true}`), call); done <- e }()
	deadline := time.After(time.Second)
	for {
		h.mu.Lock()
		users := h.states["one"].users
		h.mu.Unlock()
		if users == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("fila não admitiu")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, e := h.Invoke(context.Background(), "one", b, []byte(`{"ok":true}`), call); !errors.Is(e, plugins.ErrBusy) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, e := h.Invoke(ctx, "two", b, []byte(`{"ok":true}`), call); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
}

// TestHostCallContext força cancelamento dentro de uma chamada Go cooperativa.
func TestHostCallContext(t *testing.T) {
	l := DefaultLimits()
	l.Deadline = 30 * time.Millisecond
	h := newHost(t, l)
	_, e := h.Invoke(context.Background(), "slow", fixture(t), []byte(`{"ok":true}`), func(ctx context.Context, _ []byte) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() })
	if e == nil {
		t.Fatal("host call sem deadline")
	}
}

// TestValidation rejeita bytes inválidos, imports proibidos e assinaturas adulteradas.
func TestValidation(t *testing.T) {
	h := newHost(t, DefaultLimits())
	b := fixture(t)
	for _, data := range [][]byte{nil, []byte("wasm"), make([]byte, plugins.MaxArtifact+1), replace(b, "papo_host_v1", "evil_host_v1"), replace(b, "invoke", "broken"), replace(b, "random_get", "random_bad"), withSection(t, b, 4, []byte{1, 0x70, 0, 3}), withSection(t, b, 4, []byte{1, 0x70, 1, 3, 0x80, 0x80, 1}), withSection(t, b, 5, []byte{1, 0, 0x81, 2}), append(append([]byte(nil), b...), 8, 1, 0)} {
		if e := h.Validate(context.Background(), data); !errors.Is(e, plugins.ErrInvalidArtifact) {
			t.Fatalf("aceito: %v", e)
		}
	}
}

// replace altera somente nomes do mesmo tamanho para manter o módulo estruturalmente válido.
func replace(b []byte, from, to string) []byte {
	out := append([]byte(nil), b...)
	for i := 0; i+len(from) <= len(out); i++ {
		if string(out[i:i+len(from)]) == from {
			copy(out[i:], to)
		}
	}
	return out
}

// TestCacheBudget mantém apenas um módulo estimado e não fecha um handle em uso.
func TestCacheBudget(t *testing.T) {
	l := DefaultLimits()
	l.CacheBytes = 1 << 20
	h := newHost(t, l)
	b := fixture(t)
	first, e := h.acquire(context.Background(), b)
	if e != nil {
		t.Fatal(e)
	}
	// Custom section válida altera o digest sem mudar o código.
	other := append(append([]byte(nil), b...), 0, 3, 1, 'x', 0)
	if _, e = h.acquire(context.Background(), other); !errors.Is(e, plugins.ErrBusy) {
		t.Fatal(e)
	}
	h.release(first)
	second, e := h.acquire(context.Background(), other)
	if e != nil {
		t.Fatal(e)
	}
	h.release(second)
	if len(h.cache) != 1 || h.budget > l.CacheBytes {
		t.Fatal("cache sem limite")
	}
}

// TestInvalidJSON rejeita entrada e resposta sem aceitar UTF-8 malformado.
func TestInvalidJSON(t *testing.T) {
	h := newHost(t, DefaultLimits())
	b := fixture(t)
	if _, e := h.Invoke(context.Background(), "a", b, []byte("x"), echoCall); e == nil {
		t.Fatal("JSON aceito")
	}
	if _, e := h.Invoke(context.Background(), "a", b, json.RawMessage(`{}`), func(context.Context, []byte) ([]byte, error) { return []byte{255}, nil }); e == nil {
		t.Fatal("UTF8 aceito")
	}
}

// withSection reescreve uma seção válida para testar limites sem executar o módulo.
func withSection(t *testing.T, data []byte, id byte, replacement []byte) []byte {
	t.Helper()
	r := bytes.NewReader(data[8:])
	out := append([]byte(nil), data[:8]...)
	for r.Len() > 0 {
		section, _ := r.ReadByte()
		n, e := binary.ReadUvarint(r)
		if e != nil {
			t.Fatal(e)
		}
		payload := make([]byte, int(n))
		if _, e = io.ReadFull(r, payload); e != nil {
			t.Fatal(e)
		}
		if section == id {
			payload = replacement
		}
		out = append(out, section)
		out = binary.AppendUvarint(out, uint64(len(payload)))
		out = append(out, payload...)
	}
	return out
}
