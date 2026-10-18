package models

// AppUserProfile é a projeção pública mínima entregue a plugins; nunca inclui
// dados privados. AvatarURL é null quando o usuário não tem avatar.
type AppUserProfile struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	DisplayName string  `json:"display_name"`
	AvatarURL   *string `json:"avatar_url"`
}

// AppStorageQuota são os limites efetivos, em bytes de JSONB normalizado mais chaves.
type AppStorageQuota struct {
	MaxKeys       int `json:"max_keys"`
	MaxTotalBytes int `json:"max_total_bytes"`
	MaxValueBytes int `json:"max_value_bytes"`
}

// AppStorageQuotaOverride preserva campos omitidos; zero explícito retorna ao padrão global.
type AppStorageQuotaOverride struct {
	MaxKeys       *int `json:"max_keys,omitempty"`
	MaxTotalBytes *int `json:"max_total_bytes,omitempty"`
	MaxValueBytes *int `json:"max_value_bytes,omitempty"`
}
