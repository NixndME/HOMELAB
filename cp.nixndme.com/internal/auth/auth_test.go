package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("unexpected hash format %q", h)
	}
	if !VerifyPassword(h, "correct horse battery") {
		t.Fatal("right password rejected")
	}
	if VerifyPassword(h, "wrong") || VerifyPassword("garbage", "x") || VerifyPassword("", "") {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("hashes must be salted")
	}
}

func TestTOTPAcceptsCurrentCodeOnceOnly(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	v := &TOTPVerifier{now: func() time.Time { return now }}
	code := TOTPCode(secret, now)
	if !v.Verify(secret, code) {
		t.Fatal("current code rejected")
	}
	if v.Verify(secret, code) {
		t.Fatal("replayed code accepted")
	}
	now = now.Add(30 * time.Second)
	if !v.Verify(secret, TOTPCode(secret, now)) {
		t.Fatal("next code rejected")
	}
	if v.Verify(secret, "000000") && TOTPCode(secret, now) != "000000" {
		t.Fatal("wrong code accepted")
	}
}

func TestTOTPRFC6238Vector(t *testing.T) {
	// RFC 6238 SHA-1 test secret "12345678901234567890", T=59 -> 94287082 (8 digits); 6 digits = 287082.
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	if got := TOTPCode(secret, time.Unix(59, 0)); got != "287082" {
		t.Fatalf("got %s, want 287082", got)
	}
}

func TestLockoutLocksAfterMaxFailures(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLockout(3, time.Minute, 10*time.Minute)
	l.SetClock(func() time.Time { return now })
	l.Fail("ip")
	l.Fail("ip")
	if locked, _ := l.Locked("ip"); locked {
		t.Fatal("locked too early")
	}
	if !l.Fail("ip") {
		t.Fatal("third failure must lock")
	}
	if locked, _ := l.Locked("other"); locked {
		t.Fatal("other keys must not be locked")
	}
	now = now.Add(11 * time.Minute)
	if locked, _ := l.Locked("ip"); locked {
		t.Fatal("lock must expire")
	}
}

func TestLockoutForgetsOldFailures(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLockout(2, time.Minute, time.Hour)
	l.SetClock(func() time.Time { return now })
	l.Fail("ip")
	now = now.Add(2 * time.Minute)
	if l.Fail("ip") {
		t.Fatal("a failure outside the window must not count")
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	r := NewRateLimiter(60, 2)
	r.now = func() time.Time { return now }
	if !r.Allow("a") || !r.Allow("a") {
		t.Fatal("burst refused")
	}
	if r.Allow("a") {
		t.Fatal("over burst allowed")
	}
	now = now.Add(time.Second)
	if !r.Allow("a") {
		t.Fatal("refill not applied")
	}
}
