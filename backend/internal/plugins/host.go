// Package plugins define a fronteira de execução independente do runtime Wasm.
package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const (
	// MaxArtifact é o teto padrão do plugin.wasm (PLUGINS_MAX_WASM_BYTES).
	MaxArtifact = 16 << 20
	// MaxPayload limita envelopes de invocação e host calls.
	MaxPayload = 64 << 10
)

var (
	ErrInvalidArtifact = errors.New("artefato Wasm inválido")
	ErrBusy            = errors.New("fila de plugins cheia")
	ErrQuarantined     = errors.New("plugin em quarentena; revise e aprove o consentimento")
	ErrExecution       = errors.New("falha de execução do plugin")
)

// Call encaminha uma importação para serviços autenticados da instalação.
type Call func(context.Context, []byte) ([]byte, error)

// Module é o código de um plugin com o SHA-256 dos seus bytes, calculado uma
// vez quando a versão é carregada. O host usa Digest como chave do cache de
// compilação, sem rehashear o código a cada invocação.
type Module struct {
	Digest string
	Code   []byte
}

// NewModule calcula o digest de code.
func NewModule(code []byte) Module {
	sum := sha256.Sum256(code)
	return Module{Digest: hex.EncodeToString(sum[:]), Code: code}
}

// PluginHost permite substituir o adaptador embutido por um processo isolado.
// Invoke não compartilha memória de guest e mantém efeitos concluídos antes de erro.
type PluginHost interface {
	Validate(context.Context, []byte) error
	Invoke(context.Context, string, Module, []byte, Call) ([]byte, error)
	Reset(string)
	Close(context.Context) error
}

type memoryKey struct{}

// WithMemoryLimit seleciona o teto aprovado sem expor wazero ao domínio.
func WithMemoryLimit(ctx context.Context, mb int) context.Context {
	return context.WithValue(ctx, memoryKey{}, mb)
}

// MemoryLimit retorna o teto aprovado, com 16 MiB como padrão.
func MemoryLimit(ctx context.Context) int {
	if v, ok := ctx.Value(memoryKey{}).(int); ok && v > 0 {
		return v
	}
	return 16
}
