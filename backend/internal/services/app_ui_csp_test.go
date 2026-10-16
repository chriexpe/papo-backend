package services

import (
	"context"
	"testing"
)

func TestPluginUICSPConfiguredOrigin(t *testing.T) {
	for _, test := range []struct{ base, want string }{
		{"https://apps.example:8443/prefix/", "sandbox allow-scripts allow-forms; default-src 'self' https://apps.example:8443"},
		{"http://127.0.0.1:18087/", "sandbox allow-scripts allow-forms; default-src 'self' http://127.0.0.1:18087"},
	} {
		got, err := PluginUICSP(context.Background(), PluginSurfaceBundle{}, test.base)
		if err != nil || got != test.want {
			t.Fatalf("%q: %q, %v", test.base, got, err)
		}
	}
	for _, base := range []string{"", "/relative", "file:///ui", "https://user:password@apps.example", "https://apps.example;https://evil.example", "https://apps.example/\nscript-src *"} {
		if got, err := PluginUICSP(context.Background(), PluginSurfaceBundle{}, base); err == nil {
			t.Fatalf("invalid origin %q accepted: %q", base, got)
		}
	}
}
