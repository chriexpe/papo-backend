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
	z, err := zip.OpenReader(archive)
	if err != nil {
		return empty, err
	}
	defer z.Close()
	if len(z.File) > c.Files*2+1 {
		return empty, fmt.Errorf("muitas entradas ZIP")
	}
	staging, err := os.MkdirTemp(directory, ".extract-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(staging)
	var id string
	seen := map[string]bool{}
	var total int64
	for _, f := range z.File {
		if err = ctx.Err(); err != nil {
			return empty, err
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
			if err = os.MkdirAll(target, 0700); err != nil {
				return empty, err
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
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return empty, err
		}
		src, err := f.Open()
		if err != nil {
			return empty, err
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			src.Close()
			return empty, err
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
	b, err := Load(ctx, filepath.Join(staging, id), c, v)
	if err != nil {
		return empty, err
	}
	if strings.TrimSuffix(filepath.Base(archive), ".papoplugin") != id {
		return empty, fmt.Errorf("nome do pacote deve coincidir com id")
	}
	destination := filepath.Join(directory, id)
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		return empty, fmt.Errorf("plugin já existe")
	}
	if err = os.Rename(b.Directory, destination); err != nil {
		return empty, err
	}
	b.Directory = destination
	return b, nil
}

// Pack escreve um ZIP determinístico em temporário fora da pasta validada.
func Pack(ctx context.Context, dir, output string, c Config, v Validator) error {
	b, err := Load(ctx, dir, c, v)
	if err != nil {
		return err
	}
	out, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if filepath.Ext(out) != ".papoplugin" {
		return fmt.Errorf("saída deve usar .papoplugin")
	}
	if out == b.Directory || strings.HasPrefix(out, b.Directory+string(os.PathSeparator)) {
		return fmt.Errorf("saída deve ficar fora da pasta")
	}
	f, err := os.CreateTemp(filepath.Dir(out), ".pack-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	z := zip.NewWriter(f)
	root, err := os.OpenRoot(dir)
	if err != nil {
		f.Close()
		return err
	}
	defer root.Close()
	for _, name := range b.Files {
		dst, err := z.CreateHeader(&zip.FileHeader{Name: path.Join(b.Manifest.ID, name), Method: zip.Deflate})
		if err != nil {
			z.Close()
			f.Close()
			return err
		}
		src, err := root.Open(name)
		if err != nil {
			z.Close()
			f.Close()
			return err
		}
		_, err = io.Copy(dst, io.LimitReader(src, fileLimit(name, c)+1))
		src.Close()
		if err != nil {
			z.Close()
			f.Close()
			return err
		}
	}
	if err = z.Close(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Detecta alteração durante o empacotamento antes de publicar.
	check, err := Load(ctx, dir, c, v)
	if err != nil {
		return err
	}
	if check.Digest != b.Digest {
		return fmt.Errorf("pasta alterada durante pack")
	}
	verifyDir, err := os.MkdirTemp(filepath.Dir(out), ".pack-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(verifyDir)
	// O nome temporário precisa seguir o mesmo contrato id.papoplugin da descoberta.
	packed := filepath.Join(verifyDir, b.Manifest.ID+".papoplugin")
	if err = os.Rename(f.Name(), packed); err != nil {
		return err
	}
	extracted, err := Extract(ctx, packed, verifyDir, c, v)
	if err != nil {
		return err
	}
	if extracted.Digest != b.Digest {
		return fmt.Errorf("conteúdo do ZIP difere do digest validado")
	}
	return os.Rename(packed, out)
}
