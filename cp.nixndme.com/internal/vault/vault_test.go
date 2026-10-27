package vault

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func newVault(t *testing.T) (*Vault, string) {
	t.Helper()
	dir := t.TempDir()
	v, err := Open(filepath.Join(dir, "blobs"), filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	return v, dir
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func readAll(t *testing.T, v *Vault, id string, size int64) ([]byte, error) {
	t.Helper()
	r, err := v.Reader(id, size)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

func TestAppendInChunksRoundTrips(t *testing.T) {
	v, _ := newVault(t)
	id := NewID()
	data := randBytes(t, 3*RecordSize+12345)
	var records uint64
	for off := 0; off < len(data); off += 1500000 { // chunk size not aligned with records
		end := min(off+1500000, len(data))
		n, _, err := v.Append(id, records, bytes.NewReader(data[off:end]), int64(end-off))
		if err != nil {
			t.Fatal(err)
		}
		records += n
	}
	got, err := readAll(t, v, id, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("decrypted data differs")
	}
}

func TestStoredBytesAreNotPlaintext(t *testing.T) {
	v, dir := newVault(t)
	id := NewID()
	secret := []byte("apiVersion: v1\nkind: Secret\npassword: hunter2hunter2\n")
	if _, _, err := v.Append(id, 0, bytes.NewReader(secret), int64(len(secret))); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "blobs", id[:2], id))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("hunter2")) {
		t.Fatal("plaintext found on disk")
	}
}

func TestTamperingIsDetected(t *testing.T) {
	v, dir := newVault(t)
	id := NewID()
	data := randBytes(t, 4096)
	if _, _, err := v.Append(id, 0, bytes.NewReader(data), 4096); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "blobs", id[:2], id)
	raw, _ := os.ReadFile(p)
	raw[len(raw)-5] ^= 0xff
	os.WriteFile(p, raw, 0o600)
	if _, err := readAll(t, v, id, 4096); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestTruncationIsDetected(t *testing.T) {
	v, _ := newVault(t)
	id := NewID()
	data := randBytes(t, 2*RecordSize)
	if _, _, err := v.Append(id, 0, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(t, v, id, int64(len(data))+1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt for short blob, got %v", err)
	}
}

func TestBlobsCannotBeSwapped(t *testing.T) {
	v, dir := newVault(t)
	a, b := NewID(), NewID()
	data := randBytes(t, 100)
	v.Append(a, 0, bytes.NewReader(data), 100)
	raw, _ := os.ReadFile(filepath.Join(dir, "blobs", a[:2], a))
	os.MkdirAll(filepath.Join(dir, "blobs", b[:2]), 0o700)
	os.WriteFile(filepath.Join(dir, "blobs", b[:2], b), raw, 0o600)
	if _, err := readAll(t, v, b, 100); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("copied blob must fail authentication, got %v", err)
	}
}

func TestAppendOverLimitLeavesBlobUnchanged(t *testing.T) {
	v, _ := newVault(t)
	id := NewID()
	first := randBytes(t, 1000)
	if _, _, err := v.Append(id, 0, bytes.NewReader(first), 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.Append(id, 1, bytes.NewReader(randBytes(t, 2000)), 1000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	got, err := readAll(t, v, id, 1000)
	if err != nil || !bytes.Equal(got, first) {
		t.Fatalf("blob changed after a rejected append: %v", err)
	}
}

func TestKeyIsReusedAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	v1, _ := Open(filepath.Join(dir, "b"), key)
	id := NewID()
	v1.Append(id, 0, bytes.NewReader([]byte("hello")), 5)
	v2, err := Open(filepath.Join(dir, "b"), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readAll(t, v2, id, 5)
	if err != nil || string(got) != "hello" {
		t.Fatalf("reopen failed: %q %v", got, err)
	}
	st, _ := os.Stat(key)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, want 0600", st.Mode().Perm())
	}
}

func TestInvalidIDsAreRejected(t *testing.T) {
	v, _ := newVault(t)
	for _, id := range []string{"", "../../etc/passwd", "short", NewID() + "x"} {
		if _, _, err := v.Append(id, 0, bytes.NewReader([]byte("x")), 1); err == nil {
			t.Fatalf("id %q accepted", id)
		}
	}
}
