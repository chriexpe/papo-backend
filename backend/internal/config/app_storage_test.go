package config

import "testing"

// TestAppStorageDefaults valida defaults, env e relações entre os três limites.
func TestAppStorageDefaults(t *testing.T) {
	for _, key := range []string{"PLUGINS_STORAGE_MAX_KEYS", "PLUGINS_STORAGE_MAX_TOTAL_BYTES", "PLUGINS_STORAGE_MAX_VALUE_BYTES"} {
		t.Setenv(key, "")
	}
	q, e := AppStorageDefaults()
	if e != nil || q.MaxKeys != 256 || q.MaxTotalBytes != 1<<20 || q.MaxValueBytes != 16<<10 {
		t.Fatal(q, e)
	}
	t.Setenv("PLUGINS_STORAGE_MAX_KEYS", "3")
	t.Setenv("PLUGINS_STORAGE_MAX_TOTAL_BYTES", "1024")
	t.Setenv("PLUGINS_STORAGE_MAX_VALUE_BYTES", "512")
	q, e = AppStorageDefaults()
	if e != nil || q.MaxKeys != 3 || q.MaxValueBytes != 512 {
		t.Fatal(q, e)
	}
	for _, bad := range []string{"0", "-1", "inválido", "100001"} {
		t.Setenv("PLUGINS_STORAGE_MAX_KEYS", bad)
		if _, e = AppStorageDefaults(); e == nil {
			t.Fatal("quota inválida aceita", bad)
		}
	}
	t.Setenv("PLUGINS_STORAGE_MAX_KEYS", "3")
	t.Setenv("PLUGINS_STORAGE_MAX_VALUE_BYTES", "2048")
	if _, e = AppStorageDefaults(); e == nil {
		t.Fatal("valor maior que total")
	}
}
