// Package plugins define a fronteira de execução independente do runtime Wasm.
package plugins

import (
	"context"
	"errors"
)

const (
	MaxArtifact = 2 << 20
	MaxPayload  = 64 << 10
)

var (
	ErrInvalidArtifact = errors.New("artefato Wasm inválido")
	ErrBusy            = errors.New("fila de plugins cheia")
	ErrQuarantined     = errors.New("plugin em quarentena; envie novamente um artefato validado")
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
