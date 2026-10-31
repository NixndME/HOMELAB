package config

import (
	"slices"
	"testing"
)

func TestLiveSTUNFromEnv(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value string
		want  []string
	}{
		{"unset uses the default", false, "", []string{defaultLiveSTUN}},
		{"empty means none", true, "", nil},
		{"none means none", true, "none", nil},
		{"list", true, "stun:a.example:3478, stuns:b.example:5349", []string{"stun:a.example:3478", "stuns:b.example:5349"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("CP_LIVE_STUN", c.value)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.LiveSTUN, c.want) {
				t.Fatalf("LiveSTUN = %q, want %q", cfg.LiveSTUN, c.want)
			}
		})
	}
}

func TestLiveSTUNRejectsTURNAndJunk(t *testing.T) {
	for _, v := range []string{"turn:relay.example:3478", "http://x"} {
		t.Setenv("CP_LIVE_STUN", v)
		if _, err := Load(); err == nil {
			t.Fatalf("%q should be refused", v)
		}
	}
}
