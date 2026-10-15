package storage

import (
	"errors"
	"papo/internal/models"
	"testing"
)

// TestChannelViewStorage protege contra escrita após perda de autorização e vínculos órfãos.
func TestChannelViewStorage(t *testing.T) {
	wipeAppTables(t)
	u := newTestUser(t)
	ch := newTestChannel(t)
	a, err := CreateApp(testCtx(), "map", "view", "Mapa", u.ID, models.AppCapabilities{AttachViews: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveChannelView(testCtx(), u.ID, a.ID, ch.ID, "", "https://8.8.8.8/", 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := AddAppScope(testCtx(), a.ID, ch.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	v, err := SaveChannelView(testCtx(), u.ID, a.ID, ch.ID, "", "https://8.8.8.8/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveAppScope(testCtx(), a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := GetChannelView(testCtx(), ch.ID, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := AddAppScope(testCtx(), a.ID, ch.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveChannelView(testCtx(), u.ID, a.ID, ch.ID, "", "https://8.8.8.8/", 0); err != nil {
		t.Fatal(err)
	}
	if err := RevokeApp(testCtx(), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveChannelView(testCtx(), u.ID, a.ID, ch.ID, "", "https://8.8.8.8/", 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	var count int
	if err := GetDB().QueryRow("SELECT count(*) FROM channel_app_views WHERE app_installation_id=$1", a.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
