// Package ids generates unguessable alphanumeric identifiers from crypto/rand.
package ids

import (
	"crypto/rand"
	"math/big"
)

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// ShareLength gives 22 base62 characters, about 131 bits of entropy.
const ShareLength = 22

// TokenLength gives 43 base62 characters, about 256 bits of entropy.
const TokenLength = 43

// New returns n random base62 characters. It panics only if the OS entropy source fails,
// which leaves no safe way to continue.
func New(n int) string {
	max := big.NewInt(int64(len(alphabet)))
	out := make([]byte, n)
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic("ids: crypto/rand failed: " + err.Error())
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out)
}

// Share returns a new share identifier.
func Share() string { return New(ShareLength) }

// Token returns a new secret for sessions, CSRF and unlock grants.
func Token() string { return New(TokenLength) }

// Valid reports whether s is exactly n base62 characters, so malformed
// input never reaches the database or the filesystem.
func Valid(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
