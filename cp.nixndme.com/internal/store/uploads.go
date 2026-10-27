package store

// Upload is a chunked upload that has not been attached to a share yet.
type Upload struct {
	BlobID    string
	Name      string
	MIME      string
	Declared  int64
	Received  int64
	Records   uint64
	NextChunk int64
	CreatedAt int64
}

// Complete reports whether every declared byte has arrived.
func (u Upload) Complete() bool { return u.Received == u.Declared }

// CreateUpload registers a new chunked upload.
func (s *Store) CreateUpload(u Upload) error {
	_, err := s.db.Exec(`INSERT INTO uploads(blob_id, name, mime, declared, created_at) VALUES(?,?,?,?,?)`,
		u.BlobID, u.Name, u.MIME, u.Declared, s.Now())
	return err
}

// UploadByID loads an upload.
func (s *Store) UploadByID(blobID string) (Upload, error) {
	var u Upload
	err := s.db.QueryRow(`SELECT blob_id, name, mime, declared, received, records, next_chunk, created_at
	  FROM uploads WHERE blob_id = ?`, blobID).
		Scan(&u.BlobID, &u.Name, &u.MIME, &u.Declared, &u.Received, &u.Records, &u.NextChunk, &u.CreatedAt)
	return u, notFound(err)
}

// AdvanceUpload records a stored chunk. It only succeeds if chunk is the expected next index,
// which keeps chunks strictly ordered even if the browser retries.
func (s *Store) AdvanceUpload(blobID string, chunk int64, records uint64, bytes int64) error {
	return s.exec(`UPDATE uploads SET next_chunk = next_chunk + 1, records = records + ?, received = received + ?
	  WHERE blob_id = ? AND next_chunk = ?`, records, bytes, blobID, chunk)
}

// DeleteUpload forgets an upload (the caller deletes the blob).
func (s *Store) DeleteUpload(blobID string) error {
	_, err := s.db.Exec(`DELETE FROM uploads WHERE blob_id = ?`, blobID)
	return err
}

// StaleUploads lists uploads started before cutoff that never became a share.
func (s *Store) StaleUploads(cutoff int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT blob_id FROM uploads WHERE created_at < ?`, cutoff)
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
	return out, rows.Err()
}

// CreateGrant stores an unlock grant (password entered, or burn-after-reading revealed).
func (s *Store) CreateGrant(tokenHash string, sharePK, expiresAt int64) error {
	_, err := s.db.Exec(`INSERT INTO grants(token_hash, share_pk, expires_at) VALUES(?,?,?)`, tokenHash, sharePK, expiresAt)
	return err
}

// GrantValid reports whether the grant exists for this share and has not expired.
func (s *Store) GrantValid(tokenHash string, sharePK int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM grants WHERE token_hash = ? AND share_pk = ? AND expires_at > ?`,
		tokenHash, sharePK, s.Now()).Scan(&n)
	return n > 0, err
}

// Access is one row of a share's access log.
type Access struct {
	At     int64
	IP     string
	UA     string
	Action string
	OK     bool
}

// LogAccess appends to the share's access log.
func (s *Store) LogAccess(sharePK int64, a Access) error {
	_, err := s.db.Exec(`INSERT INTO access_log(share_pk, at, ip, ua, action, ok) VALUES(?,?,?,?,?,?)`,
		sharePK, s.Now(), a.IP, a.UA, a.Action, a.OK)
	return err
}

// AccessLog returns the newest entries for a share.
func (s *Store) AccessLog(sharePK int64, limit int) ([]Access, error) {
	rows, err := s.db.Query(`SELECT at, ip, ua, action, ok FROM access_log WHERE share_pk = ? ORDER BY at DESC, id DESC LIMIT ?`,
		sharePK, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Access
	for rows.Next() {
		var a Access
		if err := rows.Scan(&a.At, &a.IP, &a.UA, &a.Action, &a.OK); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PurgeExpired removes expired sessions and grants.
func (s *Store) PurgeExpired() error {
	now := s.Now()
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM grants WHERE expires_at <= ?`, now)
	return err
}
