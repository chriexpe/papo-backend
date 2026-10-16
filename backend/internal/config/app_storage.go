package config

import (
	"fmt"
	"os"
	"papo/internal/models"
	"strconv"
)

// AppStorageDefaults valida padrões globais antes de servir ou gravar dados.
func AppStorageDefaults() (models.AppStorageQuota, error) {
	q := models.AppStorageQuota{MaxKeys: 256, MaxTotalBytes: 1 << 20, MaxValueBytes: 16 << 10}
	for env, p := range map[string]*int{"PLUGINS_STORAGE_MAX_KEYS": &q.MaxKeys, "PLUGINS_STORAGE_MAX_TOTAL_BYTES": &q.MaxTotalBytes, "PLUGINS_STORAGE_MAX_VALUE_BYTES": &q.MaxValueBytes} {
		if raw := os.Getenv(env); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n < 1 {
				return q, fmt.Errorf("%s inválido", env)
			}
			*p = n
		}
	}
	return q, ValidateAppStorageQuota(q)
}

// ValidateAppStorageQuota limita aritmética e mantém os valores dentro do transporte do ABI.
func ValidateAppStorageQuota(q models.AppStorageQuota) error {
	if q.MaxKeys < 1 || q.MaxKeys > 100000 || q.MaxTotalBytes < 1 || q.MaxTotalBytes > 1<<30 || q.MaxValueBytes < 1 || q.MaxValueBytes > 64<<10 || q.MaxValueBytes > q.MaxTotalBytes {
		return fmt.Errorf("quota de armazenamento inválida")
	}
	return nil
}

// EffectiveAppStorageQuota aplica somente overrides positivos; zero/nil herda o padrão.
func EffectiveAppStorageQuota(o models.AppStorageQuotaOverride) (models.AppStorageQuota, error) {
	q, e := AppStorageDefaults()
	if e != nil {
		return q, e
	}
	for _, v := range []struct {
		p   *int
		out *int
	}{{o.MaxKeys, &q.MaxKeys}, {o.MaxTotalBytes, &q.MaxTotalBytes}, {o.MaxValueBytes, &q.MaxValueBytes}} {
		if v.p != nil {
			if *v.p < 0 {
				return q, fmt.Errorf("quota negativa")
			}
			if *v.p > 0 {
				*v.out = *v.p
			}
		}
	}
	return q, ValidateAppStorageQuota(q)
}
