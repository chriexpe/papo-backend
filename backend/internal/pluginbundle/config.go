// Package pluginbundle valida pastas e pacotes sem executar código guest.
package pluginbundle

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config reúne limites de descoberta, política global de permissões e limites
// de uso dos plugins. É lida uma única vez no startup (LoadConfig) e rejeita
// valores inválidos em vez de ampliar permissões silenciosamente.
type Config struct {
	// Directory é a pasta observada (PLUGINS_DIR). Entradas que começam com
	// "." são ignoradas pela descoberta, inclusive os snapshots aprovados.
	Directory string
	HotReload bool

	// Tetos de tamanho de pasta e arquivos, em bytes.
	WasmBytes     int64
	FolderBytes   int64
	ManifestBytes int64
	AssetBytes    int64
	IconBytes     int64
	ReadBytes     int64
	Files         int

	DefaultMemoryMB int
	MaxMemoryMB     int

	// Allowed/Blocked: política global aplicada antes do consentimento.
	Allowed []string
	Blocked []string

	// InteractionTimeout limita a espera de on_interaction e on_ui_call.
	InteractionTimeout time.Duration
	// InviteTTL é a validade de um convite enviado por invite.send.
	InviteTTL time.Duration
	// CleanupInterval é o intervalo da limpeza de tokens e convites expirados.
	CleanupInterval time.Duration
	// Limits são os tetos por minuto das chamadas humanas e do guest.
	Limits Limits
}

// Limits são tetos por minuto, aplicados em memória por chave (usuário,
// sessão ou instalação).
type Limits struct {
	InteractionsPerUser   int
	InteractionsPerPlugin int
	LaunchPerUser         int
	RPCPerSession         int
	RPCPerPlugin          int
	InvitesPerSender      int
	InvitesPerTarget      int
	UsersResolvePerPlugin int
}

// DefaultConfig retorna os padrões usados quando nenhuma variável é definida.
func DefaultConfig() Config {
	return Config{
		Directory:          "plugins",
		WasmBytes:          16 << 20,
		FolderBytes:        64 << 20,
		ManifestBytes:      64 << 10,
		AssetBytes:         4 << 20,
		IconBytes:          2 << 20,
		ReadBytes:          32 << 10,
		Files:              512,
		DefaultMemoryMB:    16,
		MaxMemoryMB:        64,
		Allowed:            []string{"send_messages", "read_messages", "post_app_embeds", "private_storage", "send_invites"},
		Blocked:            []string{},
		InteractionTimeout: 3 * time.Second,
		InviteTTL:          24 * time.Hour,
		CleanupInterval:    time.Minute,
		Limits: Limits{
			InteractionsPerUser:   30,
			InteractionsPerPlugin: 120,
			LaunchPerUser:         60,
			RPCPerSession:         120,
			RPCPerPlugin:          600,
			InvitesPerSender:      10,
			InvitesPerTarget:      5,
			UsersResolvePerPlugin: 120,
		},
	}
}

// LoadConfig lê as variáveis PLUGINS_* sobre DefaultConfig.
func LoadConfig() (Config, error) {
	c := DefaultConfig()
	if s := os.Getenv("PLUGINS_DIR"); s != "" {
		c.Directory = s
	}
	if s := os.Getenv("PLUGINS_HOT_RELOAD"); s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return c, fmt.Errorf("PLUGINS_HOT_RELOAD inválido")
		}
		c.HotReload = v
	}

	sizes := map[string]*int64{
		"PLUGINS_MAX_WASM_BYTES":     &c.WasmBytes,
		"PLUGINS_MAX_FOLDER_BYTES":   &c.FolderBytes,
		"PLUGINS_MAX_MANIFEST_BYTES": &c.ManifestBytes,
		"PLUGINS_MAX_ASSET_BYTES":    &c.AssetBytes,
		"PLUGINS_MAX_ICON_BYTES":     &c.IconBytes,
		"PLUGINS_ASSET_READ_BYTES":   &c.ReadBytes,
	}
	for key, p := range sizes {
		v, ok, err := positiveEnv(key, 1<<30)
		if err != nil {
			return c, err
		}
		if ok {
			*p = int64(v)
		}
	}

	ints := map[string]*int{
		"PLUGINS_MAX_FILES":                &c.Files,
		"PLUGINS_DEFAULT_MEMORY_MB":        &c.DefaultMemoryMB,
		"PLUGINS_MAX_MEMORY_MB":            &c.MaxMemoryMB,
		"PLUGINS_INTERACTIONS_PER_USER":    &c.Limits.InteractionsPerUser,
		"PLUGINS_INTERACTIONS_PER_PLUGIN":  &c.Limits.InteractionsPerPlugin,
		"PLUGINS_LAUNCH_PER_USER":          &c.Limits.LaunchPerUser,
		"PLUGINS_RPC_PER_SESSION":          &c.Limits.RPCPerSession,
		"PLUGINS_RPC_PER_PLUGIN":           &c.Limits.RPCPerPlugin,
		"PLUGINS_INVITES_PER_SENDER":       &c.Limits.InvitesPerSender,
		"PLUGINS_INVITES_PER_TARGET":       &c.Limits.InvitesPerTarget,
		"PLUGINS_USERS_RESOLVE_PER_PLUGIN": &c.Limits.UsersResolvePerPlugin,
	}
	for key, p := range ints {
		v, ok, err := positiveEnv(key, 1<<30)
		if err != nil {
			return c, err
		}
		if ok {
			*p = v
		}
	}

	durations := []struct {
		key  string
		unit time.Duration
		p    *time.Duration
	}{
		{"PLUGINS_INTERACTION_TIMEOUT_MS", time.Millisecond, &c.InteractionTimeout},
		{"PLUGINS_INVITE_TTL_SECONDS", time.Second, &c.InviteTTL},
		{"PLUGINS_CLEANUP_SECONDS", time.Second, &c.CleanupInterval},
	}
	for _, d := range durations {
		v, ok, err := positiveEnv(d.key, 1<<30)
		if err != nil {
			return c, err
		}
		if ok {
			*d.p = time.Duration(v) * d.unit
		}
	}

	if c.ReadBytes > 32<<10 || c.DefaultMemoryMB > c.MaxMemoryMB || c.MaxMemoryMB > 256 || c.Files > 100000 {
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

// positiveEnv lê um inteiro positivo de key; ok=false quando ausente.
func positiveEnv(key string, max int) (int, bool, error) {
	s := os.Getenv(key)
	if s == "" {
		return 0, false, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > max {
		return 0, false, fmt.Errorf("%s inválido", key)
	}
	return v, true, nil
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
