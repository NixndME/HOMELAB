package store

import (
	"database/sql"
	"strings"
)

// Share kinds.
const (
	KindText = "text"
	KindFile = "file"
	KindLink = "link"
)

// Share is one item: a paste, a set of files, or a link.
type Share struct {
	PK            int64
	Slug          string
	Kind          string
	Title         string
	Language      string
	LinkURL       string
	CreatedAt     int64
	ExpiresAt     int64
	Public        bool
	PassHash      string
	Burn          bool
	MaxDownloads  int64
	Downloads     int64
	Views         int64
	ConsumedUntil int64
	Size          int64
	EndedAt       int64
	EndReason     string
	Files         []File
}

// File roles.
const (
	RoleText = "text" // the share's pasted text
	RoleFile = "file" // an attached file
)

// File is one stored blob belonging to a share.
type File struct {
	ID     int64
	BlobID string
	Name   string
	MIME   string
	Size   int64
	Role   string
}

// Text returns the share's pasted text, if it has one. A share can hold text, attached
// files and links all at once; Kind only names its main part.
func (sh Share) Text() (File, bool) {
	for _, f := range sh.Files {
		if f.Role == RoleText {
			return f, true
		}
	}
	return File{}, false
}

// Attachments returns the attached files (everything except the pasted text).
func (sh Share) Attachments() []File {
	var out []File
	for _, f := range sh.Files {
		if f.Role != RoleText {
			out = append(out, f)
		}
	}
	return out
}

// Links returns the URLs of a link share (stored one per line in link_url).
func (sh Share) Links() []string {
	if sh.LinkURL == "" {
		return nil
	}
	return strings.Split(sh.LinkURL, "\n")
}

// Ended reports whether the share's content is already gone.
func (sh Share) Ended() bool { return sh.EndedAt != 0 }

// Available reports whether the content may still be served at unix time now. Cleanup runs
// on an interval, so this is checked on every request rather than trusting EndedAt alone.
func (sh Share) Available(now int64) bool {
	switch {
	case sh.Ended(), sh.ExpiresAt <= now:
		return false
	case sh.ConsumedUntil != 0 && sh.ConsumedUntil <= now:
		return false
	case sh.MaxDownloads > 0 && sh.Downloads >= sh.MaxDownloads:
		return false
	}
	return true
}

const shareCols = `pk, slug, kind, title, language, link_url, created_at, expires_at, public, pass_hash,
  burn, max_downloads, downloads, views, consumed_until, size, ended_at, end_reason`

type scanner interface{ Scan(...any) error }

func scanShare(r scanner) (Share, error) {
	var sh Share
	err := r.Scan(&sh.PK, &sh.Slug, &sh.Kind, &sh.Title, &sh.Language, &sh.LinkURL, &sh.CreatedAt,
		&sh.ExpiresAt, &sh.Public, &sh.PassHash, &sh.Burn, &sh.MaxDownloads, &sh.Downloads, &sh.Views,
		&sh.ConsumedUntil, &sh.Size, &sh.EndedAt, &sh.EndReason)
	return sh, err
}

// CreateShare inserts a share and its files, and removes the finished uploads they came from.
func (s *Store) CreateShare(sh Share) (Share, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Share{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO shares(slug, kind, title, language, link_url, created_at, expires_at, public,
	  pass_hash, burn, max_downloads, size) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		sh.Slug, sh.Kind, sh.Title, sh.Language, sh.LinkURL, sh.CreatedAt, sh.ExpiresAt, sh.Public,
		sh.PassHash, sh.Burn, sh.MaxDownloads, sh.Size)
	if err != nil {
		return Share{}, err
	}
	pk, err := res.LastInsertId()
	if err != nil {
		return Share{}, err
	}
	for i, f := range sh.Files {
		role := f.Role
		if role == "" {
			role = RoleFile
		}
		if _, err := tx.Exec(`INSERT INTO files(share_pk, blob_id, name, mime, size, position, role) VALUES(?,?,?,?,?,?,?)`,
			pk, f.BlobID, f.Name, f.MIME, f.Size, i, role); err != nil {
			return Share{}, err
		}
		if _, err := tx.Exec(`DELETE FROM uploads WHERE blob_id = ?`, f.BlobID); err != nil {
			return Share{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Share{}, err
	}
	created := sh
	created.PK = pk
	return created, nil
}

// ShareBySlug loads a share and its files.
func (s *Store) ShareBySlug(slug string) (Share, error) {
	sh, err := scanShare(s.db.QueryRow(`SELECT `+shareCols+` FROM shares WHERE slug = ?`, slug))
	if err != nil {
		return Share{}, notFound(err)
	}
	files, err := s.filesFor(sh.PK)
	if err != nil {
		return Share{}, err
	}
	sh.Files = files
	return sh, nil
}

func (s *Store) filesFor(pk int64) ([]File, error) {
	rows, err := s.db.Query(`SELECT id, blob_id, name, mime, size, role FROM files WHERE share_pk = ? ORDER BY position`, pk)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.BlobID, &f.Name, &f.MIME, &f.Size, &f.Role); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFilter selects which shares the dashboard shows.
type ListFilter struct {
	Status string // "active", "ended" or "" for all
	Query  string // matched against title, file names and link
}

// ListShares returns shares newest first, each with its files.
func (s *Store) ListShares(f ListFilter) ([]Share, error) {
	where := []string{"1=1"}
	var args []any
	switch f.Status {
	case "active":
		where = append(where, "ended_at = 0")
	case "ended":
		where = append(where, "ended_at <> 0")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		where = append(where, `(title LIKE ? ESCAPE '\' OR link_url LIKE ? ESCAPE '\' OR
		  EXISTS (SELECT 1 FROM files WHERE files.share_pk = shares.pk AND files.name LIKE ? ESCAPE '\'))`)
		args = append(args, like, like, like)
	}
	rows, err := s.db.Query(`SELECT `+shareCols+` FROM shares WHERE `+strings.Join(where, " AND ")+
		` ORDER BY created_at DESC, pk DESC LIMIT 1000`, args...)
	if err != nil {
		return nil, err
	}
	var out []Share
	for rows.Next() {
		sh, err := scanShare(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, sh)
	}
	rows.Close() // release the single connection before the per-share file queries
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		files, err := s.filesFor(out[i].PK)
		if err != nil {
			return nil, err
		}
		out[i].Files = files
	}
	return out, nil
}

func (s *Store) exec(q string, args ...any) error {
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPublic turns the public link on or off.
func (s *Store) SetPublic(pk int64, public bool) error {
	return s.exec(`UPDATE shares SET public = ? WHERE pk = ? AND ended_at = 0`, public, pk)
}

// SetExpiry moves the expiry time.
func (s *Store) SetExpiry(pk, expiresAt int64) error {
	return s.exec(`UPDATE shares SET expires_at = ? WHERE pk = ? AND ended_at = 0`, expiresAt, pk)
}

// SetTitle renames a share ("" = automatic name).
func (s *Store) SetTitle(pk int64, title string) error {
	return s.exec(`UPDATE shares SET title = ? WHERE pk = ? AND ended_at = 0`, title, pk)
}

// SetMaxDownloads changes the download limit (0 = unlimited).
func (s *Store) SetMaxDownloads(pk, max int64) error {
	return s.exec(`UPDATE shares SET max_downloads = ? WHERE pk = ? AND ended_at = 0`, max, pk)
}

// SetSharePassword sets or clears (empty hash) the per-share password.
func (s *Store) SetSharePassword(pk int64, hash string) error {
	return s.exec(`UPDATE shares SET pass_hash = ? WHERE pk = ? AND ended_at = 0`, hash, pk)
}

// SetSlug gives the share a new link; the old one stops working immediately.
func (s *Store) SetSlug(pk int64, slug string) error {
	if err := s.exec(`UPDATE shares SET slug = ? WHERE pk = ? AND ended_at = 0`, slug, pk); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM grants WHERE share_pk = ?`, pk)
	return err
}

// CountView records a page view.
func (s *Store) CountView(pk int64) error {
	_, err := s.db.Exec(`UPDATE shares SET views = views + 1 WHERE pk = ?`, pk)
	return err
}

// TakeDownload atomically claims one download slot. It reports false when max_downloads
// is already reached, so two racing visitors can never both get the last download.
func (s *Store) TakeDownload(pk int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE shares SET downloads = downloads + 1
	  WHERE pk = ? AND ended_at = 0 AND (max_downloads = 0 OR downloads < max_downloads)`, pk)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Consume marks a burn-after-reading share as opened. Only the first caller wins; the
// content stays readable to that visitor until `until`, then cleanup destroys it.
func (s *Store) Consume(pk, until int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE shares SET consumed_until = ? WHERE pk = ? AND consumed_until = 0 AND ended_at = 0`, until, pk)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// EndShare removes a share's content and returns the blob ids to delete from the vault.
// With keepHistory the row stays as a tombstone (title, sizes, counters); secrets are cleared.
func (s *Store) EndShare(pk int64, reason string, keepHistory bool) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	blobs, err := blobIDs(tx, pk)
	if err != nil {
		return nil, err
	}
	if keepHistory {
		_, err = tx.Exec(`UPDATE shares SET ended_at = ?, end_reason = ?, link_url = '', pass_hash = '', public = 0
		  WHERE pk = ?`, s.Now(), reason, pk)
		if err == nil {
			_, err = tx.Exec(`DELETE FROM grants WHERE share_pk = ?`, pk)
		}
	} else {
		_, err = tx.Exec(`DELETE FROM shares WHERE pk = ?`, pk)
	}
	if err != nil {
		return nil, err
	}
	return blobs, tx.Commit()
}

// DeleteShare removes a share and its history completely.
func (s *Store) DeleteShare(pk int64) ([]string, error) { return s.EndShare(pk, "deleted", false) }

func blobIDs(tx *sql.Tx, pk int64) ([]string, error) {
	rows, err := tx.Query(`SELECT blob_id FROM files WHERE share_pk = ?`, pk)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, err = tx.Exec(`DELETE FROM files WHERE share_pk = ?`, pk)
	return out, err
}

// DueShare is a share that cleanup must end, with the reason.
type DueShare struct {
	PK     int64
	Reason string
}

// DueForCleanup lists active shares whose content must be destroyed now.
func (s *Store) DueForCleanup() ([]DueShare, error) {
	rows, err := s.db.Query(`SELECT pk,
	  CASE WHEN consumed_until <> 0 AND consumed_until <= ?1 THEN 'burned'
	       WHEN max_downloads > 0 AND downloads >= max_downloads THEN 'download limit'
	       ELSE 'expired' END
	  FROM shares WHERE ended_at = 0 AND (expires_at <= ?1
	    OR (consumed_until <> 0 AND consumed_until <= ?1)
	    OR (max_downloads > 0 AND downloads >= max_downloads))`, s.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueShare
	for rows.Next() {
		var d DueShare
		if err := rows.Scan(&d.PK, &d.Reason); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClearHistory deletes all tombstones of ended shares.
func (s *Store) ClearHistory() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM shares WHERE ended_at <> 0`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeHistory deletes tombstones of shares that ended before cutoff.
func (s *Store) PurgeHistory(cutoff int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM shares WHERE ended_at <> 0 AND ended_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BlobInUse reports whether a vault blob still belongs to a share or an upload.
func (s *Store) BlobInUse(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM files WHERE blob_id = ?1) + (SELECT COUNT(*) FROM uploads WHERE blob_id = ?1)`, id).Scan(&n)
	return n > 0, err
}

// StoredBytes is the plaintext size of live shares plus in-progress uploads.
func (s *Store) StoredBytes() (int64, error) {
	var shares, uploads int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM files`).Scan(&shares); err != nil {
		return 0, err
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(declared),0) FROM uploads`).Scan(&uploads); err != nil {
		return 0, err
	}
	return shares + uploads, nil
}
