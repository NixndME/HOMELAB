package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestShareNumbersAreNeverReused(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := st.CreateShare(Share{Slug: "a", Kind: KindLink, LinkURL: "https://x", ExpiresAt: 1})
	if _, err := st.DeleteShare(a.PK); err != nil {
		t.Fatal(err)
	}
	b, _ := st.CreateShare(Share{Slug: "b", Kind: KindLink, LinkURL: "https://x", ExpiresAt: 1})
	if b.PK <= a.PK {
		t.Fatalf("number %d reused after delete (was %d)", b.PK, a.PK)
	}
}

func TestOldDatabaseIsMigratedKeepingData(t *testing.T) {
	dir := t.TempDir()
	old, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	// The shares table as created by the first release (no AUTOINCREMENT).
	_, err = old.Exec(`CREATE TABLE shares (
  pk INTEGER PRIMARY KEY,
  slug TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL CHECK (kind IN ('text','file','link')),
  title TEXT NOT NULL DEFAULT '', language TEXT NOT NULL DEFAULT '', link_url TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, public INTEGER NOT NULL DEFAULT 0,
  pass_hash TEXT NOT NULL DEFAULT '', burn INTEGER NOT NULL DEFAULT 0, max_downloads INTEGER NOT NULL DEFAULT 0,
  downloads INTEGER NOT NULL DEFAULT 0, views INTEGER NOT NULL DEFAULT 0, consumed_until INTEGER NOT NULL DEFAULT 0,
  size INTEGER NOT NULL DEFAULT 0, ended_at INTEGER NOT NULL DEFAULT 0, end_reason TEXT NOT NULL DEFAULT '');
INSERT INTO shares(pk, slug, kind, link_url, created_at, expires_at) VALUES (7, 'keep', 'link', 'https://kept', 1, 99);`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kept, err := st.ShareBySlug("keep")
	if err != nil || kept.PK != 7 || kept.LinkURL != "https://kept" {
		t.Fatalf("data lost in migration: %+v %v", kept, err)
	}
	if _, err := st.DeleteShare(7); err != nil {
		t.Fatal(err)
	}
	n, _ := st.CreateShare(Share{Slug: "new", Kind: KindLink, LinkURL: "https://x", ExpiresAt: 1})
	if n.PK <= 7 {
		t.Fatalf("migrated table still reuses numbers: %d", n.PK)
	}
}

func TestFileRolesMigrationMarksOldTextShares(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sh, _ := st.CreateShare(Share{Slug: "t", Kind: KindText, ExpiresAt: 1, Files: []File{{BlobID: "b1", Name: "paste.txt", MIME: "text/plain", Size: 1}}})
	// Simulate a database from before roles: drop the column's meaning by resetting it.
	if _, err := st.db.Exec(`ALTER TABLE files DROP COLUMN role`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.ShareBySlug("t")
	if err != nil {
		t.Fatal(err)
	}
	if txt, ok := got.Text(); !ok || txt.BlobID != "b1" || len(got.Attachments()) != 0 {
		t.Fatalf("old text share not migrated to role text: %+v (pk %d)", got.Files, sh.PK)
	}
}
