package pluginbundle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestApproveSnapshot copia somente o digest aprovado, reaproveita a cópia e
// isola a execução de alterações posteriores da pasta viva.
func TestApproveSnapshot(t *testing.T) {
	c := cfg(t)
	src := fixture(t, c)
	root := filepath.Dir(src)
	live, err := Load(context.Background(), src, c, nil)
	if err != nil {
		t.Fatal(err)
	}

	b, err := Approve(context.Background(), root, src, live.Digest, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Digest != live.Digest || b.Directory != SnapshotPath(root, live.Digest, "example") {
		t.Fatalf("cópia aprovada: %+v", b)
	}

	// A pasta viva muda: a cópia aprovada continua com os bytes antigos.
	if err := os.WriteFile(filepath.Join(src, "ui", "index.html"), []byte("<p>alterado</p>"), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := OpenSnapshot(context.Background(), root, live.Digest, "example", c, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := ReadAsset(snap.Directory, "ui/index.html", 0, 1024, c)
	if err != nil || string(data) != "<p>example</p>" {
		t.Fatal("cópia aprovada alterada", string(data), err)
	}

	// Aprovar o digest antigo com a pasta alterada reaproveita a cópia existente.
	if _, err := Approve(context.Background(), root, src, live.Digest, c, nil); err != nil {
		t.Fatal(err)
	}
	// Um digest que não é o da pasta atual é recusado sem publicar nada.
	other := strings.Repeat("0", 64)
	if _, err := Approve(context.Background(), root, src, other, c, nil); !errors.Is(err, ErrDigestMismatch) {
		t.Fatal("digest divergente aprovado", err)
	}
	if _, err := os.Stat(filepath.Join(root, ApprovedDir, other)); !os.IsNotExist(err) {
		t.Fatal("cópia divergente publicada", err)
	}

	// A poda mantém somente os digests informados e remove temporários.
	if err := os.Mkdir(filepath.Join(root, ApprovedDir, ".tmp-abandonado"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := PruneSnapshots(root, map[string]bool{live.Digest: true}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, ApprovedDir))
	if len(entries) != 1 || entries[0].Name() != live.Digest {
		t.Fatal("poda incorreta", entries)
	}
	if err := PruneSnapshots(root, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SnapshotPath(root, live.Digest, "example")); !os.IsNotExist(err) {
		t.Fatal("cópia não usada permaneceu", err)
	}
}

// TestApproveRefusesSymlinks não copia links, mesmo para dentro da pasta.
func TestApproveRefusesSymlinks(t *testing.T) {
	c := cfg(t)
	src := fixture(t, c)
	if err := os.Symlink(filepath.Join(src, "ui", "index.html"), filepath.Join(src, "ui", "link.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := Approve(context.Background(), filepath.Dir(src), src, strings.Repeat("a", 64), c, nil); err == nil {
		t.Fatal("symlink copiado")
	}
}
