// Package wasmhost implementa papo-core-v1 com wazero sem expor o runtime ao domínio.
package wasmhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"papo/internal/plugins"
)

// Limits fixa os tetos de admissão; o orçamento de cache é uma estimativa conservadora.
type Limits struct {
	Concurrency, Queue, Installations, Failures, CacheBytes int
	Deadline                                                time.Duration
}

// DefaultLimits limita trabalho compilado, instâncias vivas e espera por instalação.
func DefaultLimits() Limits { return Limits{4, 8, 1024, 3, 128 << 20, time.Second} }

type compiled struct {
	runtime     wazero.Runtime
	module      wazero.CompiledModule
	cost, users int
	used        uint64
}
type installation struct {
	gate            chan struct{}
	users, failures int
}
type invocationKey struct{}
type invocation struct {
	call  plugins.Call
	count int
}

// Host coordena cache, concorrência e quarentena sem reusar instâncias guest.
type Host struct {
	runtime  wazero.Runtime
	runtimes map[int]wazero.Runtime
	limits   Limits
	slots    chan struct{}
	compiler chan struct{}
	mu       sync.Mutex
	cache    map[string]*compiled
	states   map[string]*installation
	budget   int
	tick     uint64
}

// New inicia apenas as importações WASI necessárias ao reactor TinyGo.
func New(ctx context.Context, l Limits) (*Host, error) {
	if l.Concurrency < 1 || l.Queue < 0 || l.Installations < 1 || l.Failures < 1 || l.CacheBytes < 1 || l.Deadline <= 0 {
		return nil, fmt.Errorf("limites inválidos")
	}
	h := &Host{limits: l, slots: make(chan struct{}, l.Concurrency), compiler: make(chan struct{}, 1), cache: map[string]*compiled{}, states: map[string]*installation{}, runtimes: map[int]wazero.Runtime{}}
	r, e := h.newRuntime(ctx, 16)
	if e != nil {
		return nil, e
	}
	h.runtime = r
	h.runtimes[16] = r
	return h, nil
}

// newRuntime mantém memória limitada por configuração; admissão e cache continuam globais.
func (h *Host) newRuntime(ctx context.Context, mb int) (wazero.Runtime, error) {
	if mb < 1 || mb > 256 {
		return nil, plugins.ErrInvalidArtifact
	}
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithMemoryLimitPages(uint32(mb*16)).WithCloseOnContextDone(true))
	allowed := r.NewHostModuleBuilder("wasi_snapshot_preview1")
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(allowed)
	// encoding/json do TinyGo referencia clock/args; argumentos ficam vazios e relógio nega acesso.
	allowed.NewFunctionBuilder().WithFunc(func(context.Context, api.Module, uint32, uint64, uint32) uint32 { return 52 }).Export("clock_time_get")
	if _, e := allowed.Instantiate(ctx); e != nil {
		r.Close(ctx)
		return nil, e
	}
	if _, e := r.NewHostModuleBuilder("papo_host_v1").NewFunctionBuilder().WithFunc(h.call).Export("call").Instantiate(ctx); e != nil {
		r.Close(ctx)
		return nil, e
	}
	return r, nil
}

// signature compara tipos de parâmetros e retornos sem aceitar coerções do ABI.
func signature(f api.FunctionDefinition, p, r []api.ValueType) bool {
	return f != nil && bytes.Equal(f.ParamTypes(), p) && bytes.Equal(f.ResultTypes(), r)
}

// validate exige memória limitada pelo runtime, reactor e assinaturas exatas.
func validate(c wazero.CompiledModule) error {
	i32, i64 := api.ValueTypeI32, api.ValueTypeI64
	for _, f := range c.ImportedFunctions() {
		module, name, _ := f.Import()
		ok := module == "papo_host_v1" && name == "call" && signature(f, []api.ValueType{i32, i32, i32, i32}, []api.ValueType{i32})
		ok = ok || module == "wasi_snapshot_preview1" && name == "fd_write" && signature(f, []api.ValueType{i32, i32, i32, i32}, []api.ValueType{i32})
		ok = ok || module == "wasi_snapshot_preview1" && name == "random_get" && signature(f, []api.ValueType{i32, i32}, []api.ValueType{i32})
		ok = ok || module == "wasi_snapshot_preview1" && (name == "args_get" || name == "args_sizes_get") && signature(f, []api.ValueType{i32, i32}, []api.ValueType{i32})
		ok = ok || module == "wasi_snapshot_preview1" && name == "clock_time_get" && signature(f, []api.ValueType{i32, i64, i32}, []api.ValueType{i32})
		if !ok {
			return plugins.ErrInvalidArtifact
		}
	}
	if len(c.ImportedMemories()) != 0 || len(c.ExportedMemories()) != 1 || c.ExportedMemories()["memory"] == nil {
		return plugins.ErrInvalidArtifact
	}
	f := c.ExportedFunctions()
	if !signature(f["alloc"], []api.ValueType{i32}, []api.ValueType{i32}) || !signature(f["free"], []api.ValueType{i32}, nil) || !signature(f["invoke"], []api.ValueType{i32, i32}, []api.ValueType{i64}) || !signature(f["_initialize"], nil, nil) || f["_start"] != nil {
		return plugins.ErrInvalidArtifact
	}
	return nil
}

// acquire compila sob admissão global e mantém handles ativos fora da eviction.
func (h *Host) acquire(ctx context.Context, data []byte) (*compiled, error) {
	if len(data) == 0 || len(data) > plugins.ArtifactLimit() {
		return nil, plugins.ErrInvalidArtifact
	}
	if err := preflight(data); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	digest := fmt.Sprintf("%d:%s", plugins.MemoryLimit(ctx), hex.EncodeToString(sum[:]))
	// A espera por compilação também respeita o contexto; não segura o lock do scheduler.
	select {
	case h.compiler <- struct{}{}:
		defer func() { <-h.compiler }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	h.mu.Lock()
	h.tick++
	if c := h.cache[digest]; c != nil {
		c.users++
		c.used = h.tick
		h.mu.Unlock()
		return c, nil
	}
	cost := max(1<<20, len(data)*4)
	for h.budget+cost > h.limits.CacheBytes {
		var victim string
		var oldest *compiled
		for key, c := range h.cache {
			if c.users == 0 && (oldest == nil || c.used < oldest.used) {
				victim, oldest = key, c
			}
		}
		if oldest == nil {
			h.mu.Unlock()
			return nil, plugins.ErrBusy
		}
		oldest.module.Close(ctx)
		delete(h.cache, victim)
		h.budget -= oldest.cost
	}
	h.mu.Unlock()
	mb := plugins.MemoryLimit(ctx)
	h.mu.Lock()
	r := h.runtimes[mb]
	h.mu.Unlock()
	if r == nil {
		var e error
		r, e = h.newRuntime(ctx, mb)
		if e != nil {
			return nil, e
		}
		h.mu.Lock()
		h.runtimes[mb] = r
		h.mu.Unlock()
	}
	c, err := r.CompileModule(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", plugins.ErrInvalidArtifact, err)
	}
	if err = validate(c); err != nil {
		c.Close(ctx)
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := &compiled{runtime: r, module: c, cost: cost, users: 1, used: h.tick}
	h.cache[digest] = entry
	h.budget += cost
	return entry, nil
}

// release libera somente a referência, mantendo o código no cache limitado.
func (h *Host) release(c *compiled) { h.mu.Lock(); c.users--; h.mu.Unlock() }

// Validate usa a mesma compilação e whitelist usadas na invocação, sem executar start.
func (h *Host) Validate(ctx context.Context, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, h.limits.Deadline)
	defer cancel()
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		return plugins.ErrBusy
	}
	c, err := h.acquire(ctx, data)
	if err == nil {
		h.release(c)
	}
	return err
}

// enter reserva uma posição limitada; instalações saudáveis ociosas não são retidas.
func (h *Host) enter(id string) (*installation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.states[id]
	if s == nil {
		if len(h.states) >= h.limits.Installations {
			return nil, plugins.ErrBusy
		}
		s = &installation{gate: make(chan struct{}, 1)}
		h.states[id] = s
	}
	if s.failures >= h.limits.Failures {
		return nil, plugins.ErrQuarantined
	}
	if s.users >= h.limits.Queue+1 {
		return nil, plugins.ErrBusy
	}
	s.users++
	return s, nil
}

// leave mantém falhas consecutivas; cancelamentos enquanto se espera não contam.
func (h *Host) leave(id string, s *installation, ran, failed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.users--
	if ran {
		if failed {
			s.failures++
		} else {
			s.failures = 0
		}
	}
	if s.users == 0 && s.failures == 0 {
		delete(h.states, id)
	}
}

// Invoke serializa por instalação e impõe prazo também à espera e aos host calls.
func (h *Host) Invoke(parent context.Context, id string, artifact, input []byte, call plugins.Call) (out []byte, err error) {
	if len(input) > plugins.MaxPayload || !utf8.Valid(input) || !json.Valid(input) {
		return nil, plugins.ErrExecution
	}
	s, err := h.enter(id)
	if err != nil {
		return nil, err
	}
	ran := false
	gateHeld := false
	defer func() {
		h.leave(id, s, ran, ran && err != nil)
		h.mu.Lock()
		quarantined := s.failures >= h.limits.Failures
		h.mu.Unlock()
		if quarantined && err != nil {
			err = errors.Join(err, plugins.ErrQuarantined)
		}
		if gateHeld {
			<-s.gate
		}
	}()
	ctx, cancel := context.WithTimeout(parent, h.limits.Deadline)
	defer cancel()
	select {
	case s.gate <- struct{}{}:
		gateHeld = true
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// A falha anterior pode ter aberto o circuito enquanto esta chamada esperava.
	h.mu.Lock()
	quarantined := s.failures >= h.limits.Failures
	h.mu.Unlock()
	if quarantined {
		return nil, plugins.ErrQuarantined
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c, err := h.acquire(ctx, artifact)
	if err != nil {
		return nil, err
	}
	defer h.release(c)
	ran = true
	ctx = context.WithValue(ctx, invocationKey{}, &invocation{call: call})
	m, err := c.runtime.InstantiateModule(ctx, c.module, wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize"))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", plugins.ErrExecution, err)
	}
	defer m.Close(context.Background())
	ptr, err := m.ExportedFunction("alloc").Call(ctx, uint64(len(input)))
	if err != nil || len(ptr) != 1 || ptr[0] == 0 {
		return nil, plugins.ErrExecution
	}
	if !m.Memory().Write(uint32(ptr[0]), input) {
		return nil, plugins.ErrExecution
	}
	result, err := m.ExportedFunction("invoke").Call(ctx, ptr[0], uint64(len(input)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", plugins.ErrExecution, err)
	}
	p, n := uint32(result[0]>>32), uint32(result[0])
	if n > plugins.MaxPayload {
		return nil, plugins.ErrExecution
	}
	data, ok := m.Memory().Read(p, n)
	if !ok || !utf8.Valid(data) || !json.Valid(data) {
		return nil, plugins.ErrExecution
	}
	var envelope struct {
		OK *bool `json:"ok"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.OK == nil {
		return nil, plugins.ErrExecution
	}
	out = append([]byte(nil), data...)
	// free afeta apenas esta instância e também está sujeito ao deadline.
	if _, err = m.ExportedFunction("free").Call(ctx, ptr[0]); err != nil {
		return nil, plugins.ErrExecution
	}
	if uint64(p) != ptr[0] {
		if _, err = m.ExportedFunction("free").Call(ctx, uint64(p)); err != nil {
			return nil, plugins.ErrExecution
		}
	}
	return out, nil
}

// call valida todos os offsets antes de copiar; um excesso encerra esta instância.
func (h *Host) call(ctx context.Context, m api.Module, p, n, out, capacity uint32) uint32 {
	state, _ := ctx.Value(invocationKey{}).(*invocation)
	if state == nil || state.call == nil {
		panic("host call fora de invoke")
	}
	state.count++
	if state.count > 32 || n > plugins.MaxPayload || capacity > plugins.MaxPayload {
		panic("limite de host calls")
	}
	data, ok := m.Memory().Read(p, n)
	if !ok || !utf8.Valid(data) || !json.Valid(data) {
		panic("host request inválido")
	}
	if _, ok = m.Memory().Read(out, capacity); !ok {
		panic("host response inválida")
	}
	response, err := state.call(ctx, append([]byte(nil), data...))
	if err != nil {
		panic("host call falhou")
	}
	if len(response) > plugins.MaxPayload {
		panic("limite de host response")
	}
	if len(response) > int(capacity) {
		return uint32(len(response))
	}
	if !m.Memory().Write(out, response) {
		panic("host response inválida")
	}
	return uint32(len(response))
}

// Reset permite reabilitar uma instalação com um upload administrativo validado.
func (h *Host) Reset(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.states[id]; s != nil {
		s.failures = 0
		if s.users == 0 {
			delete(h.states, id)
		}
	}
}

// Close fecha o runtime depois que o servidor drenou requisições.
func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var first error
	for _, r := range h.runtimes {
		if e := r.Close(ctx); e != nil && first == nil {
			first = e
		}
	}
	return first
}
