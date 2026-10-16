// Package plugins define a fronteira de execução independente do runtime Wasm.
package plugins

import (
	"context"
	"errors"
	"os"
	"strconv"
)

const (
	MaxArtifact = 16 << 20
	MaxPayload  = 64 << 10
)

var (
	ErrInvalidArtifact = errors.New("artefato Wasm inválido")
	ErrBusy            = errors.New("fila de plugins cheia")
	ErrQuarantined     = errors.New("plugin em quarentena; revise e aprove o consentimento")
	ErrExecution       = errors.New("falha de execução do plugin")
)

// Call encaminha uma importação para serviços autenticados da instalação.
type Call func(context.Context, []byte) ([]byte, error)

// PluginHost permite substituir o adaptador embutido por um processo isolado.
// Invoke não compartilha memória de guest e mantém efeitos concluídos antes de erro.
type PluginHost interface {
	Validate(context.Context, []byte) error
	Invoke(context.Context, string, []byte, []byte, Call) ([]byte, error)
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

// ArtifactLimit acompanha o limite configurado para descoberta e compilação.
func ArtifactLimit() int {
	if n, e := strconv.Atoi(os.Getenv("PLUGINS_MAX_WASM_BYTES")); e == nil && n > 0 && n <= 1<<30 {
		return n
	}
	return MaxArtifact
}
