package pluginbundle

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture cria uma pasta mínima; validação ABI real é coberta pelo CLI/runtime e integração.
func fixture(t *testing.T, c Config) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "example")
	if e := os.MkdirAll(filepath.Join(dir, "ui"), 0700); e != nil {
		t.Fatal(e)
	}
	files := map[string]string{"plugin.wasm": "\x00asm\x01\x00\x00\x00", "plugin.toml": `manifest_version = 1
id = "example"
name = "Example"
version = "1.0.0"
min_papo_api = 1
[permissions]
request = []
[[surfaces]]
id = "home"
kind = "channel"
default_name = "example"
entry = "ui/index.html"
`, "ui/index.html": "<p>example</p>"}
	for name, b := range files {
		if e := os.WriteFile(filepath.Join(dir, name), []byte(b), 0600); e != nil {
			t.Fatal(e)
		}
	}
	return dir
}

// cfg evita depender do ambiente do desenvolvedor nos testes de limites.
func cfg(t *testing.T) Config {
	t.Helper()
	c, e := LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	return c
}

// TestFolderValidation rejeita manifest, paths, links e limites com o mesmo validador.
func TestFolderValidation(t *testing.T) {
	for _, tc := range []string{"missing_wasm", "missing_manifest", "unknown_field", "wrong_id", "missing_entry", "ui_type", "symlink", "special", "wasm_limit", "folder_limit", "file_limit", "manifest_limit", "asset_limit", "bad_icon"} {
		t.Run(tc, func(t *testing.T) {
			c := cfg(t)
			dir := fixture(t, c)
			manifest := filepath.Join(dir, "plugin.toml")
			b, _ := os.ReadFile(manifest)
			switch tc {
			case "missing_wasm":
				os.Remove(filepath.Join(dir, "plugin.wasm"))
			case "missing_manifest":
				os.Remove(manifest)
			case "unknown_field":
				os.WriteFile(manifest, append([]byte("unknown = 1\n"), b...), 0600)
			case "wrong_id":
				os.WriteFile(manifest, []byte(strings.Replace(string(b), `id = "example"`, `id = "other"`, 1)), 0600)
			case "missing_entry":
				os.Remove(filepath.Join(dir, "ui/index.html"))
			case "ui_type":
				os.WriteFile(filepath.Join(dir, "ui/source.go"), []byte("package main"), 0600)
			case "symlink":
				if e := os.Symlink("/etc/passwd", filepath.Join(dir, "secret")); e != nil {
					t.Fatal(e)
				}
			case "special":
				os.WriteFile(filepath.Join(dir, ".hidden"), nil, 0600)
			case "wasm_limit":
				c.WasmBytes = 7
			case "folder_limit":
				c.FolderBytes = 10
			case "file_limit":
				c.Files = 1
			case "manifest_limit":
				c.ManifestBytes = 10
			case "asset_limit":
				c.AssetBytes = 1
			case "bad_icon":
				os.WriteFile(filepath.Join(dir, "icon.png"), []byte("not PNG"), 0600)
			}
			if _, e := Load(context.Background(), dir, c, nil); e == nil {
				t.Fatal("pasta inválida aceita")
			}
		})
	}
}

// TestDigestAndAssets inclui UI/templates no digest e permite leituras parciais sem traversal.
func TestDigestAndAssets(t *testing.T) {
	c := cfg(t)
	dir := fixture(t, c)
	a, e := Load(context.Background(), dir, c, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "data.txt"), []byte("abcdef"), 0600); e != nil {
		t.Fatal(e)
	}
	b, e := Load(context.Background(), dir, c, nil)
	if e != nil || a.Digest == b.Digest {
		t.Fatal("asset não alterou digest", e)
	}
	chunk, e := ReadAsset(dir, "data.txt", 2, 3, c)
	if e != nil || string(chunk) != "cde" {
		t.Fatal(string(chunk), e)
	}
	for _, req := range []struct {
		path           string
		offset, length int64
	}{{"../data.txt", 0, 1}, {"/etc/passwd", 0, 1}, {"data.txt", -1, 1}, {"data.txt", 7, 1}, {"data.txt", 0, c.ReadBytes + 1}, {"data.txt", 0, 0}, {"..\\data.txt", 0, 1}, {"ui", 0, 1}} {
		if _, e := ReadAsset(dir, req.path, req.offset, req.length, c); e == nil {
			t.Fatal("asset inválido aceito", req)
		}
	}
}

// TestArchiveRoundTrip confirma que check/pack/discovery produzem exatamente o mesmo digest.
func TestArchiveRoundTrip(t *testing.T) {
	c := cfg(t)
	dir := fixture(t, c)
	out := filepath.Join(t.TempDir(), "example.papoplugin")
	if e := Pack(context.Background(), dir, out, c, nil); e != nil {
		t.Fatal(e)
	}
	a, e := Load(context.Background(), dir, c, nil)
	if e != nil {
		t.Fatal(e)
	}
	dest := t.TempDir()
	b, e := Extract(context.Background(), out, dest, c, nil)
	if e != nil || a.Digest != b.Digest {
		t.Fatal("digest do ZIP diferente", e)
	}
	if _, e = Extract(context.Background(), out, dest, c, nil); e == nil {
		t.Fatal("sobrescreveu pasta")
	}
}

// TestUnsafeArchives rejeita zip slip, links, duplicatas e bombas antes de publicar.
func TestUnsafeArchives(t *testing.T) {
	for _, name := range []string{"traversal", "absolute", "symlink", "duplicate", "size", "multi_root", "missing_wasm"} {
		t.Run(name, func(t *testing.T) {
			c := cfg(t)
			archive := filepath.Join(t.TempDir(), "example.papoplugin")
			f, e := os.Create(archive)
			if e != nil {
				t.Fatal(e)
			}
			z := zip.NewWriter(f)
			entry := "example/data.txt"
			content := "data"
			mode := os.FileMode(0600)
			switch name {
			case "traversal":
				entry = "example/../../escape"
			case "absolute":
				entry = "/tmp/escape"
			case "symlink":
				mode = os.ModeSymlink | 0600
			case "size":
				c.AssetBytes = 2
			}
			h := &zip.FileHeader{Name: entry, Method: zip.Deflate}
			h.SetMode(mode)
			w, e := z.CreateHeader(h)
			if e != nil {
				t.Fatal(e)
			}
			w.Write([]byte(content))
			if name == "duplicate" {
				w, _ = z.Create(entry)
				w.Write([]byte(content))
			}
			if name == "multi_root" {
				w, _ = z.Create("other/data.txt")
				w.Write([]byte(content))
			}
			z.Close()
			f.Close()
			dest := t.TempDir()
			if _, e = Extract(context.Background(), archive, dest, c, nil); e == nil {
				t.Fatal("ZIP inválido aceito")
			}
			files, _ := os.ReadDir(dest)
			if len(files) != 0 {
				t.Fatal("pasta parcial publicada")
			}
		})
	}
}

// TestConfig fecha allowlist vazia, prioridade de blocked e limites configurados.
func TestConfig(t *testing.T) {
	t.Setenv("PLUGINS_MAX_WASM_BYTES", "12345")
	t.Setenv("PLUGINS_ALLOWED_PERMISSIONS", "")
	c, e := LoadConfig()
	if e != nil || c.WasmBytes != 12345 || c.PermissionAllowed("send_messages") {
		t.Fatal(c, e)
	}
	t.Setenv("PLUGINS_ALLOWED_PERMISSIONS", "send_messages,private_storage")
	t.Setenv("PLUGINS_BLOCKED_PERMISSIONS", "private_storage")
	c, e = LoadConfig()
	if e != nil || !c.PermissionAllowed("send_messages") || c.PermissionAllowed("private_storage") {
		t.Fatal(c, e)
	}
	t.Setenv("PLUGINS_MAX_FOLDER_BYTES", "-1")
	if _, e = LoadConfig(); e == nil {
		t.Fatal("limite negativo aceito")
	}
}

// TestManifestVersionAndDefaults rejeita SemVer e defaults que não satisfazem o schema v1.
func TestManifestVersionAndDefaults(t *testing.T) {
	c := cfg(t)
	dir := fixture(t, c)
	valid, e := os.ReadFile(filepath.Join(dir, "plugin.toml"))
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range []string{"1.0.0-alpha..x", "1.0.0-01", "1.0.0+build..x"} {
		data := strings.Replace(string(valid), `version = "1.0.0"`, `version = "`+v+`"`, 1)
		if _, e := Parse([]byte(data), c); e == nil {
			t.Fatal("SemVer inválida aceita", v)
		}
	}
	for _, kind := range []string{"integer", "boolean", "channel", "https_url"} {
		data := string(valid) + "\n[[settings]]\nkey=\"invalid\"\ntype=\"" + kind + "\"\ndefault=\"bad\"\n"
		if _, e := Parse([]byte(data), c); e == nil {
			t.Fatal("default inválido aceito", kind)
		}
	}
}
