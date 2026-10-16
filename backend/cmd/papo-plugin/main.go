// papo-plugin valida e empacota pastas usando o mesmo contrato da descoberta.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"papo/internal/pluginbundle"
	"papo/internal/plugins"
	"papo/internal/wasmhost"
)

// run mantém a saída legível e códigos de falha consistentes para automação.
func run(args []string) error {
	if len(args) < 2 || (args[0] != "check" && args[0] != "pack") {
		return fmt.Errorf("uso: papo-plugin check <pasta> | pack <pasta> <arquivo.papoplugin>")
	}
	c, e := pluginbundle.LoadConfig()
	if e != nil {
		return e
	}
	ctx := context.Background()
	h, e := wasmhost.New(ctx, wasmhost.DefaultLimits())
	if e != nil {
		return e
	}
	defer h.Close(ctx)
	validate := func(ctx context.Context, data []byte, mb int) error {
		return h.Validate(plugins.WithMemoryLimit(ctx, mb), data)
	}
	if args[0] == "pack" {
		if len(args) != 3 {
			return fmt.Errorf("pack exige arquivo de saída")
		}
		return pluginbundle.Pack(ctx, args[1], args[2], c, validate)
	}
	if len(args) != 2 {
		return fmt.Errorf("check exige uma pasta")
	}
	b, e := pluginbundle.Load(ctx, args[1], c, validate)
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": b.Manifest.ID, "version": b.Manifest.Version, "digest": b.Digest, "files": len(b.Files), "valid": true})
}

// main não executa o plugin durante check ou pack.
func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
