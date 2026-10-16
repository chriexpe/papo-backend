package storage

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"papo/internal/models"
)

// TestAppStorageConstraints comprova no PostgreSQL a exclusividade de tokens remotos.
func TestAppStorageConstraints(t *testing.T) {
	wipeAppTables(t)
	u := newTestUser(t)
	hash := uuid.NewString()
	for _, tc := range []struct {
		kind  string
		hash  *string
		valid bool
	}{{"remote", &hash, true}, {"remote", nil, false}, {"view", nil, false}, {"view", &hash, false}, {"wasm", nil, false}, {"wasm", &hash, false}, {"plugin", nil, true}, {"plugin", &hash, false}, {"other", nil, false}} {
		_, err := CreateApp(testCtx(), "key", tc.kind, "App", u.ID, models.AppCapabilities{}, tc.hash)
		if (err == nil) != tc.valid {
			t.Fatalf("kind=%s hash=%v: %v", tc.kind, tc.hash, err)
		}
	}
}

// TestAppStorageLifecycle cobre cada operação e preserva atribuição após revogação.
func TestAppStorageLifecycle(t *testing.T) {
	wipeAppTables(t)
	u := newTestUser(t)
	ch := newTestChannel(t)
	hash := uuid.NewString()
	a, err := CreateApp(testCtx(), "key", "remote", "Bot", u.ID, models.AppCapabilities{SendMessages: true}, &hash)
	if err != nil {
		t.Fatal(err)
	}
	got, stored, err := GetActiveAppByHash(testCtx(), hash)
	if err != nil || stored != hash || got.ID != a.ID {
		t.Fatalf("hash: %+v %v", got, err)
	}
	if err = AddAppScope(testCtx(), a.ID, ch.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if err = AddAppScope(testCtx(), a.ID, ch.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if has, err := AppHasScope(testCtx(), a.ID, ch.ID); err != nil || !has {
		t.Fatalf("escopo: %v %v", has, err)
	}
	if err = RemoveAppScope(testCtx(), a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	if err = AddAppScope(testCtx(), a.ID, ch.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	media := newTestMedia(t, []byte("avatar-app"))
	updated, err := UpdateApp(testCtx(), a.ID, "Novo", sql.NullString{String: "Nick", Valid: true}, sql.NullString{String: media, Valid: true}, &models.AppCapabilities{ReadMessages: true})
	if err != nil || updated.Name != "Novo" || updated.DisplayName == nil || updated.AvatarMedia == nil || !updated.Capabilities.ReadMessages {
		t.Fatalf("update: %+v %v", updated, err)
	}
	if _, err = UpdateApp(testCtx(), a.ID, nil, sql.NullString{}, sql.NullString{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = UpdateApp(testCtx(), a.ID, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	ext := &models.ExternalAuthor{Source: "app", ID: a.ID, Name: a.Name}
	first, created, err := CreateAppMessage(testCtx(), ch.ID, a.ID, ext, "key", "original", "", nil)
	if err != nil || !created {
		t.Fatal(err)
	}
	retry, created, err := CreateAppMessage(testCtx(), ch.ID, a.ID, ext, "key", "ignorado", "", nil)
	if err != nil || created || retry.ID != first.ID {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	// Exercita também o conflito com transação de attachments.
	attachment, err := CreateAttachment(testCtx(), models.Attachments{OriginalFileName: "file", MediaShaHash: media, CreatedBy: &u.ID})
	if err != nil {
		t.Fatal(err)
	}
	retry, created, err = CreateAppMessage(testCtx(), ch.ID, a.ID, ext, "key", "ignorado", "", []string{attachment.ID})
	if err != nil || created || retry.ID != first.ID {
		t.Fatalf("retry anexado: %+v %v", retry, err)
	}
	if _, err = GetAppMessage(testCtx(), a.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = RotateAppToken(testCtx(), a.ID, "new-hash"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = GetActiveAppByHash(testCtx(), hash); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = RevokeApp(testCtx(), a.ID); err != nil {
		t.Fatal(err)
	}
	if has, err := AppHasScope(testCtx(), a.ID, ch.ID); err != nil || has {
		t.Fatalf("escopo revogado: %v %v", has, err)
	}
	if err = AddAppScope(testCtx(), a.ID, ch.ID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = GetAppMessage(testCtx(), a.ID, "key"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, uuid.NewString()} {
		if err = RevokeApp(testCtx(), id); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if err = RotateAppToken(testCtx(), id, "hash"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err = UpdateApp(testCtx(), id, "Nome", nil, nil, nil); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
}

// TestAppStoragePagination cobre empate de timestamps sem perder instalações.
func TestAppStoragePagination(t *testing.T) {
	wipeAppTables(t)
	u := newTestUser(t)
	// Outros testes mantêm instalações sem canal; limita o conjunto explicitamente.
	if _, err := GetDB().ExecContext(testCtx(), "DELETE FROM app_installations"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := CreateApp(testCtx(), "key", "plugin", "App", u.ID, models.AppCapabilities{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := GetDB().ExecContext(testCtx(), "UPDATE app_installations SET created_at=$1", stamp); err != nil {
		t.Fatal(err)
	}
	for _, asc := range []bool{true, false} {
		seen := map[string]bool{}
		var since *time.Time
		last := ""
		for {
			page, err := ListApps(testCtx(), since, last, asc, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, a := range page {
				if seen[a.ID] {
					t.Fatal("duplicado")
				}
				seen[a.ID] = true
			}
			a := page[len(page)-1]
			since = &a.CreatedAt
			last = a.ID
		}
		if len(seen) != 5 {
			t.Fatalf("itens: %d", len(seen))
		}
	}
}
