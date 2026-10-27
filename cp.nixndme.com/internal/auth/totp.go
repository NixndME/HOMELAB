package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// RFC 6238 defaults used by every authenticator app.
const (
	totpPeriod = 30
	totpDigits = 6
	totpSkew   = 1 // accept one step either side for clock drift
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit base32 secret.
func NewTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return b32.EncodeToString(raw), nil
}

// TOTPURI is the otpauth:// URI an authenticator app scans.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "digits": {fmt.Sprint(totpDigits)}, "period": {fmt.Sprint(totpPeriod)}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// TOTPCode returns the code for secret at time t (used by tests and the setup check).
func TOTPCode(secret string, t time.Time) string {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return ""
	}
	return totpCode(key, t.Unix()/totpPeriod)
}

func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

// TOTPVerifier checks codes and refuses to accept the same time step twice (replay).
type TOTPVerifier struct {
	mu       sync.Mutex
	lastStep int64
	now      func() time.Time
}

// NewTOTPVerifier returns a verifier using the wall clock.
func NewTOTPVerifier() *TOTPVerifier { return &TOTPVerifier{now: time.Now} }

// Verify reports whether code is valid for secret right now.
func (v *TOTPVerifier) Verify(secret, code string) bool {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now().Unix() / totpPeriod
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := now + d
		if step <= v.lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCode(key, step)), []byte(code)) == 1 {
			v.lastStep = step
			return true
		}
	}
	return false
}
