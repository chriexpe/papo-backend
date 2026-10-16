// Package pluginbundle valida pastas e pacotes sem executar código guest.
package pluginbundle

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config reúne limites de descoberta e política global, lidos uma vez no startup.
type Config struct {
	Directory                                                               string
	HotReload                                                               bool
	WasmBytes, FolderBytes, ManifestBytes, AssetBytes, IconBytes, ReadBytes int64
	Files, DefaultMemoryMB, MaxMemoryMB                                     int
	Allowed, Blocked                                                        []string
}

// LoadConfig rejeita limites inválidos em vez de ampliar permissões silenciosamente.
func LoadConfig() (Config, error) {
	c := Config{Directory: "plugins", WasmBytes: 16 << 20, FolderBytes: 64 << 20, ManifestBytes: 64 << 10, AssetBytes: 4 << 20, IconBytes: 2 << 20, ReadBytes: 32 << 10, Files: 512, DefaultMemoryMB: 16, MaxMemoryMB: 64, Allowed: []string{"send_messages", "read_messages", "post_app_embeds", "private_storage", "send_invites"}}
	if s := os.Getenv("PLUGINS_DIR"); s != "" {
		c.Directory = s
	}
	if s := os.Getenv("PLUGINS_HOT_RELOAD"); s != "" {
		v, e := strconv.ParseBool(s)
		if e != nil {
			return c, e
		}
		c.HotReload = v
	}
	for key, p := range map[string]*int64{"PLUGINS_MAX_WASM_BYTES": &c.WasmBytes, "PLUGINS_MAX_FOLDER_BYTES": &c.FolderBytes, "PLUGINS_MAX_MANIFEST_BYTES": &c.ManifestBytes, "PLUGINS_MAX_ASSET_BYTES": &c.AssetBytes, "PLUGINS_MAX_ICON_BYTES": &c.IconBytes, "PLUGINS_ASSET_READ_BYTES": &c.ReadBytes} {
		if s := os.Getenv(key); s != "" {
			v, e := strconv.ParseInt(s, 10, 64)
			if e != nil || v < 1 || v > 1<<30 {
				return c, fmt.Errorf("%s inválido", key)
			}
			*p = v
		}
	}
	for key, p := range map[string]*int{"PLUGINS_MAX_FILES": &c.Files, "PLUGINS_DEFAULT_MEMORY_MB": &c.DefaultMemoryMB, "PLUGINS_MAX_MEMORY_MB": &c.MaxMemoryMB} {
		if s := os.Getenv(key); s != "" {
			v, e := strconv.Atoi(s)
			if e != nil || v < 1 {
				return c, fmt.Errorf("%s inválido", key)
			}
			*p = v
		}
	}
	if c.ReadBytes > (32<<10) || c.DefaultMemoryMB > c.MaxMemoryMB || c.MaxMemoryMB > 256 || c.Files > 100000 {
		return c, fmt.Errorf("limites de plugins inválidos")
	}
	if s, ok := os.LookupEnv("PLUGINS_ALLOWED_PERMISSIONS"); ok {
		c.Allowed = split(s)
	}
	c.Blocked = split(os.Getenv("PLUGINS_BLOCKED_PERMISSIONS"))
	for _, p := range append(append([]string{}, c.Allowed...), c.Blocked...) {
		if !knownPermission(p) {
			return c, fmt.Errorf("permissão desconhecida: %s", p)
		}
	}
	return c, nil
}

// split preserva a distinção entre allowlist vazia e configuração ausente.
func split(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// PermissionAllowed aplica o bloqueio antes do grant administrativo.
func (c Config) PermissionAllowed(p string) bool {
	for _, v := range c.Blocked {
		if v == p {
			return false
		}
	}
	for _, v := range c.Allowed {
		if v == p {
			return true
		}
	}
	return false
}

// knownPermission fecha o universo das capacidades do manifest v1.
func knownPermission(p string) bool {
	switch p {
	case "send_messages", "read_messages", "post_app_embeds", "private_storage", "send_invites":
		return true
	}
	return false
}
