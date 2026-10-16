package pluginbundle

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Extract publica atomicamente uma pasta validada e nunca sobrescreve uma existente.
func Extract(ctx context.Context, archive, directory string, c Config, v Validator) (Bundle, error) {
	var empty Bundle
	z, e := zip.OpenReader(archive)
	if e != nil {
		return empty, e
	}
	defer z.Close()
	if len(z.File) > c.Files*2+1 {
		return empty, fmt.Errorf("muitas entradas ZIP")
	}
	staging, e := os.MkdirTemp(directory, ".extract-")
	if e != nil {
		return empty, e
	}
	defer os.RemoveAll(staging)
	var id string
	seen := map[string]bool{}
	var total int64
	for _, f := range z.File {
		if e = ctx.Err(); e != nil {
			return empty, e
		}
		name := strings.TrimSuffix(f.Name, "/")
		if !SafePath(name) || f.Mode()&os.ModeSymlink != 0 || (!f.Mode().IsRegular() && !f.FileInfo().IsDir()) || f.Flags&1 != 0 {
			return empty, fmt.Errorf("entrada ZIP inválida")
		}
		parts := strings.SplitN(name, "/", 2)
		if id == "" {
			id = parts[0]
		}
		if parts[0] != id || !identifier.MatchString(id) {
			return empty, fmt.Errorf("ZIP exige uma pasta id")
		}
		lower := strings.ToLower(name)
		if seen[lower] {
			return empty, fmt.Errorf("entrada ZIP duplicada")
		}
		seen[lower] = true
		target := filepath.Join(staging, filepath.FromSlash(name))
		if f.FileInfo().IsDir() {
			if e = os.MkdirAll(target, 0700); e != nil {
				return empty, e
			}
			continue
		}
		if len(parts) != 2 {
			return empty, fmt.Errorf("arquivo fora da pasta")
		}
		limit := fileLimit(parts[1], c)
		if f.UncompressedSize64 > uint64(limit) {
			return empty, fmt.Errorf("limite ZIP excedido")
		}
		total += int64(f.UncompressedSize64)
		if total > c.FolderBytes {
			return empty, fmt.Errorf("pasta ZIP excedida")
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return empty, e
		}
		src, e := f.Open()
		if e != nil {
			return empty, e
		}
		dst, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			src.Close()
			return empty, e
		}
		n, copyErr := io.Copy(dst, io.LimitReader(src, limit+1))
		closeErr := dst.Close()
		src.Close()
		if copyErr != nil {
			return empty, copyErr
		}
		if closeErr != nil {
			return empty, closeErr
		}
		if n > limit || uint64(n) != f.UncompressedSize64 {
			return empty, fmt.Errorf("tamanho ZIP inválido")
		}
	}
	b, e := Load(ctx, filepath.Join(staging, id), c, v)
	if e != nil {
		return empty, e
	}
	if strings.TrimSuffix(filepath.Base(archive), ".papoplugin") != id {
		return empty, fmt.Errorf("nome do pacote deve coincidir com id")
	}
	destination := filepath.Join(directory, id)
	if _, e = os.Lstat(destination); !os.IsNotExist(e) {
		return empty, fmt.Errorf("plugin já existe")
	}
	if e = os.Rename(b.Directory, destination); e != nil {
		return empty, e
	}
	b.Directory = destination
	return b, nil
}

// Pack escreve um ZIP determinístico em temporário fora da pasta validada.
func Pack(ctx context.Context, dir, output string, c Config, v Validator) error {
	b, e := Load(ctx, dir, c, v)
	if e != nil {
		return e
	}
	out, e := filepath.Abs(output)
	if e != nil {
		return e
	}
	if filepath.Ext(out) != ".papoplugin" {
		return fmt.Errorf("saída deve usar .papoplugin")
	}
	if out == b.Directory || strings.HasPrefix(out, b.Directory+string(os.PathSeparator)) {
		return fmt.Errorf("saída deve ficar fora da pasta")
	}
	f, e := os.CreateTemp(filepath.Dir(out), ".pack-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	z := zip.NewWriter(f)
	root, e := os.OpenRoot(dir)
	if e != nil {
		f.Close()
		return e
	}
	defer root.Close()
	for _, name := range b.Files {
		dst, e := z.CreateHeader(&zip.FileHeader{Name: path.Join(b.Manifest.ID, name), Method: zip.Deflate})
		if e != nil {
			z.Close()
			f.Close()
			return e
		}
		src, e := root.Open(name)
		if e != nil {
			z.Close()
			f.Close()
			return e
		}
		_, e = io.Copy(dst, io.LimitReader(src, fileLimit(name, c)+1))
		src.Close()
		if e != nil {
			z.Close()
			f.Close()
			return e
		}
	}
	if e = z.Close(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	// Detecta alteração durante o empacotamento antes de publicar.
	check, e := Load(ctx, dir, c, v)
	if e != nil {
		return e
	}
	if check.Digest != b.Digest {
		return fmt.Errorf("pasta alterada durante pack")
	}
	verifyDir, e := os.MkdirTemp(filepath.Dir(out), ".pack-verify-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(verifyDir)
	// O nome temporário precisa seguir o mesmo contrato id.papoplugin da descoberta.
	packed := filepath.Join(verifyDir, b.Manifest.ID+".papoplugin")
	if e = os.Rename(f.Name(), packed); e != nil {
		return e
	}
	extracted, e := Extract(ctx, packed, verifyDir, c, v)
	if e != nil {
		return e
	}
	if extracted.Digest != b.Digest {
		return fmt.Errorf("conteúdo do ZIP difere do digest validado")
	}
	return os.Rename(packed, out)
}
