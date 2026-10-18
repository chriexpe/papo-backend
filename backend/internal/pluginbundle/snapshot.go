package pluginbundle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ApprovedDir é o subdiretório de PLUGINS_DIR com as cópias aprovadas. Começa
// com "." para ficar fora da descoberta.
const ApprovedDir = ".approved"

// ErrDigestMismatch indica que os bytes copiados não são os que o
// consentimento aprovou (a pasta mudou entre o preview e a aprovação).
var ErrDigestMismatch = errors.New("conteúdo do plugin difere do digest aprovado")

// SnapshotPath é o caminho da cópia aprovada de id com o digest informado:
// <root>/.approved/<digest>/<id>. O nome final da pasta continua sendo o id,
// como Load exige.
func SnapshotPath(root, digest, id string) string {
	return filepath.Join(root, ApprovedDir, digest, id)
}

// Approve copia a pasta src para uma cópia imutável em <root>/.approved e
// valida a cópia com Load. A cópia só é publicada quando o digest calculado
// sobre os bytes copiados é igual a want; assim uma alteração da pasta depois
// do preview nunca é aprovada. Se a cópia daquele digest já existe, ela é
// reaproveitada.
func Approve(ctx context.Context, root, src, want string, c Config, validate Validator) (Bundle, error) {
	id := filepath.Base(src)
	if existing, err := OpenSnapshot(ctx, root, want, id, c, validate); err == nil {
		return existing, nil
	}
	approved := filepath.Join(root, ApprovedDir)
	if err := os.MkdirAll(approved, 0750); err != nil {
		return Bundle{}, err
	}
	staging, err := os.MkdirTemp(approved, ".tmp-")
	if err != nil {
		return Bundle{}, err
	}
	defer os.RemoveAll(staging)

	dst := filepath.Join(staging, id)
	if err := copyFolder(ctx, src, dst, c); err != nil {
		return Bundle{}, err
	}
	b, err := Load(ctx, dst, c, validate)
	if err != nil {
		return Bundle{}, err
	}
	if b.Digest != want {
		return Bundle{}, ErrDigestMismatch
	}
	final := filepath.Join(approved, b.Digest)
	if err := os.Rename(staging, final); err != nil {
		// Outra aprovação do mesmo digest venceu a corrida: usa a dela.
		if existing, openErr := OpenSnapshot(ctx, root, want, id, c, validate); openErr == nil {
			return existing, nil
		}
		return Bundle{}, err
	}
	b.Directory = filepath.Join(final, id)
	return b, nil
}

// OpenSnapshot carrega e valida uma cópia aprovada existente, conferindo que o
// conteúdo ainda corresponde ao digest do nome.
func OpenSnapshot(ctx context.Context, root, digest, id string, c Config, validate Validator) (Bundle, error) {
	if !digestName(digest) {
		return Bundle{}, fmt.Errorf("digest inválido")
	}
	b, err := Load(ctx, SnapshotPath(root, digest, id), c, validate)
	if err != nil {
		return Bundle{}, err
	}
	if b.Digest != digest {
		return Bundle{}, ErrDigestMismatch
	}
	return b, nil
}

// PruneSnapshots remove cópias aprovadas cujo digest não está em keep, além de
// cópias temporárias abandonadas.
func PruneSnapshots(root string, keep map[string]bool) error {
	approved := filepath.Join(root, ApprovedDir)
	entries, err := os.ReadDir(approved)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] {
			continue
		}
		if !digestName(name) && !strings.HasPrefix(name, ".tmp-") {
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(approved, name)))
	}
	return errors.Join(errs...)
}

// digestName aceita somente o SHA-256 hexadecimal minúsculo produzido por Load.
func digestName(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// copyFolder copia somente diretórios e arquivos regulares de src para dst,
// recusando symlinks e respeitando os tetos de arquivos e bytes da pasta. A
// validação completa fica para o Load da cópia.
func copyFolder(ctx context.Context, src, dst string, c Config) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("pasta inválida")
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := os.Mkdir(dst, 0750); err != nil {
		return err
	}
	var total int64
	files := 0
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if !SafePath(name) || d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("caminho inválido: %s", name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if d.IsDir() {
			return os.Mkdir(target, 0750)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("arquivo especial: %s", name)
		}
		files++
		if files > c.Files {
			return fmt.Errorf("limite da pasta excedido")
		}
		in, err := root.Open(name)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0440)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(out, io.LimitReader(in, c.FolderBytes-total+1))
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		total += n
		if total > c.FolderBytes {
			return fmt.Errorf("limite da pasta excedido")
		}
		return nil
	})
}
