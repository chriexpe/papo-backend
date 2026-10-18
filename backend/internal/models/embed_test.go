package models

import (
	"encoding/json"
	"testing"
)

// TestNativeEmbedJSONPreserved garante que cards nativos com imagem/texto não viram frames.
func TestNativeEmbedJSONPreserved(t *testing.T) {
	title, description, url, color := "Título", "Texto", "https://example.org/image.png", "#112233"
	e := Embed{ID: "native", SourceType: "custom", FetchMethod: "manual", Title: &title, Description: &description, Color: &color, Image: &EmbedMedia{URL: &url}}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	type original Embed
	want, err := json.Marshal(original(e))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatal(string(raw), string(want))
	}
	var payload map[string]json.RawMessage
	json.Unmarshal(raw, &payload)
	for _, key := range []string{"title", "description", "image", "color"} {
		if payload[key] == nil {
			t.Fatal(key)
		}
	}
	for _, key := range []string{"surface", "entry", "size", "load", "display", "poster"} {
		if payload[key] != nil {
			t.Fatal(key)
		}
	}
}
