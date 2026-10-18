package storage

import (
	"database/sql"
	"errors"
	"testing"

	"papo/internal/models"
)

// A SQL error after the write models a later insert failing in the same tx.
// No production hook or timing-dependent context cancellation is needed.
func failTransaction(t *testing.T, write func(*sql.Tx)) {
	t.Helper()
	tx, err := GetDB().BeginTx(testCtx(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	write(tx)
	if _, err := tx.ExecContext(testCtx(), "SELECT 1 / 0"); err == nil {
		t.Fatal("expected forced SQL failure")
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("aborted transaction committed")
	}
}

func TestMessageInsertRollback(t *testing.T) {
	owner := newTestUser(t)
	channel := newTestChannel(t)
	var id string
	failTransaction(t, func(tx *sql.Tx) {
		m, inserted, err := createMessageInTx(testCtx(), tx, channel.ID, owner.ID, "", "", nil, "", "rollback", "", nil)
		if err != nil || !inserted {
			t.Fatalf("inserted=%v err=%v", inserted, err)
		}
		id = m.ID
	})
	if _, err := GetMessageByID(testCtx(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("message survived: %v", err)
	}
}

func TestTransactionalWritesRollback(t *testing.T) {
	owner := newTestUser(t)
	channel := newTestChannel(t)
	original, err := CreateMessage(testCtx(), channel.ID, owner.ID, "original", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	unicode := "👍"
	t.Run("update", func(t *testing.T) {
		failTransaction(t, func(tx *sql.Tx) {
			if _, err := updateMessageInTx(testCtx(), tx, original.ID, models.Message{Content: strPtr("edited")}); err != nil {
				t.Fatal(err)
			}
		})
		got, err := GetMessageByID(testCtx(), original.ID)
		if err != nil || got.Content == nil || *got.Content != "original" || got.EditedAt != nil {
			t.Fatalf("update survived: %+v %v", got, err)
		}
	})
	t.Run("add reaction", func(t *testing.T) {
		failTransaction(t, func(tx *sql.Tx) {
			_, created, count, err := addReactionInTx(testCtx(), tx, original.ID, owner.ID, nil, &unicode)
			if err != nil || !created || count != 1 {
				t.Fatalf("created=%v count=%d err=%v", created, count, err)
			}
		})
		if count := countMessageReactions(t, original.ID); count != 0 {
			t.Fatalf("reaction survived: %d", count)
		}
	})
	if _, _, _, err := AddReaction(testCtx(), original.ID, owner.ID, nil, &unicode); err != nil {
		t.Fatal(err)
	}
	t.Run("remove reaction", func(t *testing.T) {
		failTransaction(t, func(tx *sql.Tx) {
			count, err := removeReactionInTx(testCtx(), tx, original.ID, owner.ID, nil, &unicode)
			if err != nil || count != 0 {
				t.Fatalf("count=%d err=%v", count, err)
			}
		})
		if count := countMessageReactions(t, original.ID); count != 1 {
			t.Fatalf("removal survived: %d", count)
		}
	})
	for _, name := range []string{"delete message", "delete channel"} {
		t.Run(name, func(t *testing.T) {
			failTransaction(t, func(tx *sql.Tx) {
				var err error
				if name == "delete message" {
					err = deleteMessageInTx(testCtx(), tx, original.ID)
				} else {
					err = deleteChannelInTx(testCtx(), tx, channel.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			})
			if _, err := GetChannelByID(testCtx(), channel.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := GetMessageByID(testCtx(), original.ID); err != nil {
				t.Fatal(err)
			}
			if count := countMessageReactions(t, original.ID); count != 1 {
				t.Fatalf("cascade survived: %d", count)
			}
		})
	}
}

func TestMessageAttachmentFailureRollsBackInsert(t *testing.T) {
	owner := newTestUser(t)
	channel := newTestChannel(t)
	_, _, err := CreateMessageAs(testCtx(), channel.ID, owner.ID, "", nil, "", "rollback", "", []string{"invalid-uuid"})
	if err == nil {
		t.Fatal("expected attachment linking failure")
	}
	var count int
	if err := GetDB().QueryRowContext(testCtx(), "SELECT COUNT(*) FROM messages WHERE channel_id=$1", channel.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("insert survived: %d", count)
	}
}
