package services

import "testing"

func TestResolvePushMentions(t *testing.T) {
	const ana = "7a1abb0c-47d7-4ebd-8c25-924e7ded85b4"
	const gone = "00000000-0000-4000-8000-000000000000"
	lookups := 0
	lookup := func(id string) string {
		lookups++
		if id == ana {
			return "Ana"
		}
		return ""
	}

	got := resolvePushMentions("oi <@"+ana+">, e @"+ana+" de novo, <@"+gone+"> @everyone", lookup)
	want := "oi @Ana, e @Ana de novo, <@" + gone + "> @everyone"
	if got != want {
		t.Fatalf("resolvePushMentions() = %q, want %q", got, want)
	}
	if lookups != 2 {
		t.Fatalf("lookups = %d, want 2 (one per distinct id)", lookups)
	}
}

func TestResolvePushMentionsIsCaseInsensitive(t *testing.T) {
	got := resolvePushMentions("<@7A1ABB0C-47D7-4EBD-8C25-924E7DED85B4>", func(id string) string {
		if id == "7a1abb0c-47d7-4ebd-8c25-924e7ded85b4" {
			return "Ana"
		}
		return ""
	})
	if got != "@Ana" {
		t.Fatalf("got %q", got)
	}
}

func TestPushDisplayNamePrefersNickname(t *testing.T) {
	nick := "Aninha"
	blank := "  "
	if got := pushDisplayName(&nick, "ana"); got != "Aninha" {
		t.Fatalf("got %q", got)
	}
	if got := pushDisplayName(&blank, "ana"); got != "ana" {
		t.Fatalf("got %q", got)
	}
	if got := pushDisplayName(nil, "ana"); got != "ana" {
		t.Fatalf("got %q", got)
	}
}
