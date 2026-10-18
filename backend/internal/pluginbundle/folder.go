package pluginbundle

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	_ "golang.org/x/image/webp"
	"image"
	_ "image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"papo/internal/plugins"
)

// Bundle contém a definição validada e bytes do módulo, nunca uma instância executável.
type Bundle struct {
	Manifest  Manifest `json:"manifest"`
	Directory string   `json:"-"`
	Digest    string   `json:"digest"`
	Files     []string `json:"files"`
	Module    []byte   `json:"-"`
	// ModuleDigest é o SHA-256 de plugin.wasm, chave do cache de compilação.
	ModuleDigest string `json:"-"`
}

// Code devolve o módulo com o digest calculado no Load.
func (b Bundle) Code() plugins.Module {
	return plugins.Module{Digest: b.ModuleDigest, Code: b.Module}
}

// Validator valida ABI/imports e limites sem instanciar.
type Validator func(context.Context, []byte, int) error

// Load usa exatamente a mesma validação na descoberta, check e pack.
func Load(ctx context.Context, dir string, c Config, validate Validator) (Bundle, error) {
	var b Bundle
	info, err := os.Lstat(dir)
	if err != nil {
		return b, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return b, fmt.Errorf("pasta inválida")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return b, err
	}
	defer root.Close()
	var files []string
	var total int64
	entries := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		entries++
		if entries > c.Files*2+1 {
			return fmt.Errorf("limite de entradas excedido")
		}
		if !SafePath(name) || d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("caminho inválido: %s", name)
		}
		if d.IsDir() {
			return nil
		}
		i, err := d.Info()
		if err != nil {
			return err
		}
		if !i.Mode().IsRegular() {
			return fmt.Errorf("arquivo especial: %s", name)
		}
		if strings.HasPrefix(name, "ui/") && !UIExtension(name) {
			return fmt.Errorf("tipo UI inválido: %s", name)
		}
		limit := fileLimit(name, c)
		if i.Size() > limit {
			return fmt.Errorf("limite de arquivo: %s", name)
		}
		total += i.Size()
		if total > c.FolderBytes || len(files) >= c.Files {
			return fmt.Errorf("limite da pasta excedido")
		}
		files = append(files, name)
		return nil
	})
	if err != nil {
		return b, err
	}
	sort.Strings(files)
	h := sha256.New()
	fold := map[string]bool{}
	for _, name := range files {
		if fold[strings.ToLower(name)] {
			return b, fmt.Errorf("colisão de nomes")
		}
		fold[strings.ToLower(name)] = true
		f, err := root.Open(name)
		if err != nil {
			return b, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return b, err
		}
		if !info.Mode().IsRegular() {
			f.Close()
			return b, fmt.Errorf("arquivo alterado")
		}
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(name))))
		h.Write([]byte(name))
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(info.Size())))
		var data []byte
		if name == "plugin.wasm" || name == "plugin.toml" || name == "icon.png" || name == "icon.webp" {
			data, err = io.ReadAll(io.LimitReader(f, fileLimit(name, c)+1))
			if err == nil && int64(len(data)) > fileLimit(name, c) {
				err = fmt.Errorf("limite excedido")
			}
			h.Write(data)
		} else {
			var n int64
			n, err = io.Copy(h, io.LimitReader(f, fileLimit(name, c)+1))
			if err == nil && (n > fileLimit(name, c) || n != info.Size()) {
				err = fmt.Errorf("arquivo alterado")
			}
		}
		f.Close()
		if err != nil {
			return b, err
		}
		switch name {
		case "plugin.wasm":
			b.Module = data
			b.ModuleDigest = plugins.NewModule(data).Digest
		case "plugin.toml":
			b.Manifest, err = Parse(data, c)
			if err != nil {
				return b, err
			}
		case "icon.png", "icon.webp":
			format := "png"
			if name == "icon.webp" {
				format = "webp"
			}
			conf, actual, err := image.DecodeConfig(strings.NewReader(string(data)))
			if err != nil || actual != format || conf.Width < 1 || conf.Height < 1 || conf.Width > 4096 || conf.Height > 4096 {
				return b, fmt.Errorf("ícone inválido")
			}
			if _, _, err = image.Decode(strings.NewReader(string(data))); err != nil {
				return b, fmt.Errorf("ícone inválido: %w", err)
			}
		}
	}
	if b.Module == nil || b.Manifest.ID == "" {
		return b, fmt.Errorf("plugin.wasm e plugin.toml obrigatórios")
	}
	if filepath.Base(dir) != b.Manifest.ID {
		return b, fmt.Errorf("id difere da pasta")
	}
	for _, s := range b.Manifest.Surfaces {
		if s.Poster != "" && !contains(files, s.Poster) {
			return b, fmt.Errorf("poster inexistente: %s", s.Poster)
		}
		if s.Entry != "" && !contains(files, s.Entry) {
			return b, fmt.Errorf("entry inexistente: %s", s.Entry)
		}
	}
	for _, a := range b.Manifest.UserActions {
		if a.Icon != "" && !contains(files, a.Icon) {
			return b, fmt.Errorf("ícone inexistente: %s", a.Icon)
		}
	}
	if validate != nil {
		if err = validate(ctx, b.Module, b.Manifest.Runtime.MaxMemoryMB); err != nil {
			return b, err
		}
	}
	b.Directory, err = filepath.Abs(dir)
	if err != nil {
		return b, err
	}
	b.Files = files
	b.Digest = hex.EncodeToString(h.Sum(nil))
	return b, nil
}

// contains compara nomes completos sem prefixos ambíguos.
func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// fileLimit atribui limites especiais aos arquivos do contrato.
func fileLimit(name string, c Config) int64 {
	switch name {
	case "plugin.wasm":
		return c.WasmBytes
	case "plugin.toml":
		return c.ManifestBytes
	case "icon.png", "icon.webp":
		return c.IconBytes
	}
	return c.AssetBytes
}

// ReadAsset abre somente um arquivo regular confinado, com leitura parcial limitada.
func ReadAsset(dir, name string, offset, length int64, c Config) ([]byte, error) {
	if !SafePath(name) || offset < 0 || length < 1 || length > c.ReadBytes {
		return nil, fmt.Errorf("asset request inválido")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink recusado")
		}
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !i.Mode().IsRegular() || i.Size() > c.AssetBytes || offset > i.Size() {
		return nil, fmt.Errorf("asset inválido")
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, length))
}
