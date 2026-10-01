package websocket

import (
	"testing"
	"time"
)

func TestActivityStoreFallsBackAcrossConnections(t *testing.T) {
	store := NewActivityStore()
	first := ActivityPayload{Kind: "playing", Name: "Hades"}
	second := ActivityPayload{Kind: "working", Name: "Zed"}

	got, changed := store.Set("user", "desktop-a", &first)
	if !changed || got == nil || got.Name != "Hades" { t.Fatalf("first=%+v changed=%v", got, changed) }
	got, changed = store.Set("user", "desktop-b", &second)
	if !changed || got == nil || got.Name != "Zed" { t.Fatalf("second=%+v changed=%v", got, changed) }
	got, changed = store.Set("user", "desktop-b", nil)
	if !changed || got == nil || got.Name != "Hades" { t.Fatalf("fallback=%+v changed=%v", got, changed) }
	got, changed = store.Set("user", "desktop-a", nil)
	if !changed || got != nil { t.Fatalf("clear=%+v changed=%v", got, changed) }
}

func TestActivityStoreSnapshotIsDeterministicAndCloned(t *testing.T) {
	store := NewActivityStore()
	details := "song"
	start := time.Unix(1_700_000_000, 0).UTC()
	activity := ActivityPayload{Kind:"listening", Name:"Spotify", Details:&details, StartedAt:&start}
	store.Set("zeta", "a", &activity)
	store.Set("alpha", "b", &activity)
	snapshot := store.Snapshot()
	if len(snapshot)!=2 || snapshot[0].UserID!="alpha" || snapshot[1].UserID!="zeta" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	*snapshot[0].Activity.Details = "changed"
	again := store.Snapshot()
	if again[0].Activity.Details == nil || *again[0].Activity.Details != "song" {
		t.Fatalf("snapshot leaked mutation: %+v", again[0])
	}
}

func TestActivityEventDirection(t *testing.T) {
	if !EventTypeActivityUpdate.IsInbound() { t.Fatal("activity_update must be inbound") }
	if EventTypeActivitySync.IsInbound() { t.Fatal("activity_sync must be outbound only") }
}
