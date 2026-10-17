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
	c, err := pluginbundle.LoadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	limits := wasmhost.DefaultLimits()
	limits.ArtifactBytes = int(c.WasmBytes)
	h, err := wasmhost.New(ctx, limits)
	if err != nil {
		return err
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
	b, err := pluginbundle.Load(ctx, args[1], c, validate)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": b.Manifest.ID, "version": b.Manifest.Version, "digest": b.Digest, "files": len(b.Files), "valid": true})
}

// main não executa o plugin durante check ou pack.
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
