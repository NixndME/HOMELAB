package live

import (
	"testing"
	"time"
)

func TestTURNCredentialMatchesCoturnScheme(t *testing.T) {
	// Expected password computed independently:
	//   printf '%s' "1700086400:cp" | openssl dgst -sha1 -hmac "<secret>" -binary | base64
	now := time.Unix(1700000000, 0)
	user, pass := TURNCredential("test-secret-0123456789abcdef0123456789", now, 24*time.Hour)
	if user != "1700086400:cp" {
		t.Fatalf("username = %q", user)
	}
	if pass != "sQVlyc5lzLznUh2oWUmcxztb220=" {
		t.Fatalf("password = %q", pass)
	}
}

func TestTURNCredentialDependsOnSecretAndTime(t *testing.T) {
	now := time.Unix(1700000000, 0)
	u1, p1 := TURNCredential("secret-one-0123456789abcdef012345", now, time.Hour)
	_, p2 := TURNCredential("secret-two-0123456789abcdef012345", now, time.Hour)
	u3, _ := TURNCredential("secret-one-0123456789abcdef012345", now.Add(time.Second), time.Hour)
	if p1 == p2 {
		t.Fatal("different secrets gave the same password")
	}
	if u1 == u3 {
		t.Fatal("different times gave the same username")
	}
}
