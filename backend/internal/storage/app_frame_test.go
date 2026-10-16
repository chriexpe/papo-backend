package storage

import (
	"papo/internal/models"
	"testing"
)

// TestAppFrameStorageChecks exige dimensões/políticas válidas sem fixar teto de configuração no SQL.
func TestAppFrameStorageChecks(t *testing.T) {
	wipeAppTables(t)
	u := newTestUser(t)
	a, err := CreateApp(testCtx(), "frames", "wasm", "Frames", u.ID, models.AppCapabilities{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := GetDB().ExecContext(testCtx(), "DELETE FROM embeds WHERE app_installation_id=$1", a.ID); err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		name          string
		width, height any
		load, display any
		valid         bool
	}{
		{"inline_click", 560, 420, "click", "inline", true},
		{"inline_visible", 560, 420, "visible", "inline", true},
		{"overlay_click", 560, 420, "click", "overlay", true},
		{"configurable_max", 800, 600, "click", "inline", true},
		{"overlay_visible", 560, 420, "visible", "overlay", false},
		{"null_width", nil, 420, "click", "inline", false},
		{"null_height", 560, nil, "click", "inline", false},
		{"zero_width", 0, 420, "click", "inline", false},
		{"negative_height", 560, -1, "click", "inline", false},
		{"null_load", 560, 420, nil, "inline", false},
		{"null_display", 560, 420, "click", nil, false},
		{"bad_load", 560, 420, "eager", "inline", false},
		{"bad_display", 560, 420, "click", "popup", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetDB().Exec(`INSERT INTO embeds(source_type,fetch_method,app_installation_id,app_surface_id,app_width,app_height,app_load,app_display)
 VALUES ('app','manual',$1,'result',$2,$3,$4,$5)`, a.ID, tc.width, tc.height, tc.load, tc.display)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}
