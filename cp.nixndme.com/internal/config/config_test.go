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

const testTURNSecret = "0123456789abcdef0123456789abcdef"

func TestLiveTURNFromEnv(t *testing.T) {
	t.Setenv("CP_LIVE_TURN", "turn:192.0.2.10:3478?transport=udp, turn:192.0.2.10:3478?transport=tcp")
	t.Setenv("CP_LIVE_TURN_SECRET", testTURNSecret)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"turn:192.0.2.10:3478?transport=udp", "turn:192.0.2.10:3478?transport=tcp"}
	if !slices.Equal(cfg.LiveTURN, want) || cfg.LiveTURNSecret != testTURNSecret {
		t.Fatalf("LiveTURN = %q secret = %q", cfg.LiveTURN, cfg.LiveTURNSecret)
	}
}

func TestLiveTURNIsOffByDefault(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LiveTURN) != 0 || cfg.LiveTURNSecret != "" {
		t.Fatalf("relay configured by default: %q", cfg.LiveTURN)
	}
}

func TestLiveTURNRejectsBadSettings(t *testing.T) {
	cases := []struct{ name, urls, secret string }{
		{"stun url in the relay list", "stun:192.0.2.10:3478", testTURNSecret},
		{"junk url", "http://192.0.2.10", testTURNSecret},
		{"missing secret", "turn:192.0.2.10:3478", ""},
		{"short secret", "turn:192.0.2.10:3478", "too-short"},
		{"secret without relay", "", testTURNSecret},
		{"shell characters in secret", "turn:192.0.2.10:3478", testTURNSecret + "$(id)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CP_LIVE_TURN", c.urls)
			t.Setenv("CP_LIVE_TURN_SECRET", c.secret)
			if _, err := Load(); err == nil {
				t.Fatal("should be refused")
			}
		})
	}
}
