package ids

import "testing"

func TestShareIDsAreUniqueAndAlphanumeric(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		id := Share()
		if !Valid(id, ShareLength) {
			t.Fatalf("invalid id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestValidRejectsBadInput(t *testing.T) {
	for _, s := range []string{"", "abc", "aaaaaaaaaaaaaaaaaaaaa/", "aaaaaaaaaaaaaaaaaaaaa.", "ääääääääääää"} {
		if Valid(s, ShareLength) {
			t.Fatalf("%q accepted", s)
		}
	}
}
