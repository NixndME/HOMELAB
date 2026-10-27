// Package vault stores blobs on disk encrypted at rest with AES-256-GCM.
//
// A blob file is a sequence of records:
//
//	[uint32 big-endian ciphertext length][12-byte nonce][ciphertext + 16-byte tag]
//
// Each record seals up to RecordSize plaintext bytes with AAD = blobID || record index, so
// records cannot be swapped between blobs or reordered. Truncation is caught by the reader,
// which compares the decrypted length with the expected plaintext size from the database.
package vault

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cpshare/internal/ids"
)

// RecordSize is the plaintext size of one sealed record.
const RecordSize = 1 << 20

// BlobIDLength is the length of a blob identifier.
const BlobIDLength = 32

const (
	nonceSize  = 12
	headerSize = 4 + nonceSize
	keySize    = 32
)

var (
	// ErrCorrupt means a blob failed authentication or has the wrong length.
	ErrCorrupt = errors.New("vault: blob is corrupt or was tampered with")
	// ErrTooLarge means more data arrived than the caller allowed.
	ErrTooLarge = errors.New("vault: data exceeds the allowed size")
)

// Vault is an encrypted blob directory.
type Vault struct {
	dir  string
	aead cipher.AEAD
}

// Open loads (or on first use creates) the 32-byte key at keyFile and returns a vault rooted at dir.
func Open(dir, keyFile string) (*Vault, error) {
	key, err := loadOrCreateKey(keyFile)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Vault{dir: dir, aead: aead}, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		key, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(key) != keySize {
			return nil, fmt.Errorf("vault: key file %s must hold %d hex-encoded bytes", path, keySize)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("vault: read key: %w", err)
	}
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("vault: write key: %w", err)
	}
	return key, nil
}

// NewID returns a fresh blob identifier.
func NewID() string { return ids.New(BlobIDLength) }

func (v *Vault) path(id string) (string, error) {
	if !ids.Valid(id, BlobIDLength) {
		return "", fmt.Errorf("vault: invalid blob id")
	}
	return filepath.Join(v.dir, id[:2], id), nil
}

func (v *Vault) aad(id string, index uint64) []byte {
	out := make([]byte, 0, len(id)+8)
	out = append(out, id...)
	return binary.BigEndian.AppendUint64(out, index)
}

// Append encrypts everything read from r onto the end of blob id, numbering records from
// firstRecord, and refuses more than limit bytes. It returns the records and plaintext bytes
// written. On any error the file is truncated back to its previous length, so a failed chunk
// upload leaves the blob exactly as it was.
func (v *Vault) Append(id string, firstRecord uint64, r io.Reader, limit int64) (uint64, int64, error) {
	p, err := v.path(id)
	if err != nil {
		return 0, 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return 0, 0, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, 0, err
	}
	start, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return 0, 0, err
	}
	records, n, err := v.writeRecords(f, id, firstRecord, io.LimitReader(r, limit+1), limit)
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		_ = f.Truncate(start)
		f.Close()
		return 0, 0, err
	}
	return records, n, f.Close()
}

func (v *Vault) writeRecords(w io.Writer, id string, index uint64, r io.Reader, limit int64) (uint64, int64, error) {
	buf := make([]byte, RecordSize)
	out := make([]byte, 0, headerSize+RecordSize+v.aead.Overhead())
	var records uint64
	var total int64
	for {
		m, readErr := io.ReadFull(r, buf)
		if m > 0 {
			total += int64(m)
			if total > limit {
				return 0, 0, ErrTooLarge
			}
			nonce := make([]byte, nonceSize)
			if _, err := rand.Read(nonce); err != nil {
				return 0, 0, err
			}
			out = append(out[:4], nonce...)
			out = v.aead.Seal(out, nonce, buf[:m], v.aad(id, index+records))
			binary.BigEndian.PutUint32(out[:4], uint32(len(out)-headerSize))
			if _, err := w.Write(out); err != nil {
				return 0, 0, err
			}
			records++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			return records, total, nil
		}
		if readErr != nil {
			return 0, 0, readErr
		}
	}
}

// Reader returns a stream of the decrypted blob. expected is the plaintext size recorded
// when the blob was written; any mismatch yields ErrCorrupt.
func (v *Vault) Reader(id string, expected int64) (io.ReadCloser, error) {
	p, err := v.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &reader{v: v, id: id, f: f, src: bufio.NewReaderSize(f, 64<<10), expected: expected}, nil
}

type reader struct {
	v        *Vault
	id       string
	f        *os.File
	src      *bufio.Reader
	index    uint64
	plain    []byte
	total    int64
	expected int64
	done     bool
}

func (r *reader) Read(p []byte) (int, error) {
	for len(r.plain) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.plain)
	r.plain = r.plain[n:]
	return n, nil
}

func (r *reader) next() error {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r.src, hdr[:]); err != nil {
		if err == io.EOF && r.total == r.expected {
			r.done = true
			return nil
		}
		return ErrCorrupt
	}
	size := binary.BigEndian.Uint32(hdr[:4])
	if size > RecordSize+uint32(r.v.aead.Overhead()) {
		return ErrCorrupt
	}
	sealed := make([]byte, size)
	if _, err := io.ReadFull(r.src, sealed); err != nil {
		return ErrCorrupt
	}
	plain, err := r.v.aead.Open(sealed[:0], hdr[4:], sealed, r.v.aad(r.id, r.index))
	if err != nil {
		return ErrCorrupt
	}
	r.index++
	r.total += int64(len(plain))
	if r.total > r.expected {
		return ErrCorrupt
	}
	r.plain = plain
	return nil
}

func (r *reader) Close() error { return r.f.Close() }

// Delete removes a blob. A missing blob is not an error.
func (v *Vault) Delete(id string) error {
	p, err := v.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
