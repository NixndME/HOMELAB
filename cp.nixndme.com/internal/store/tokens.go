package store

// APIToken lets a script (the cpup command, curl) create shares without a browser login.
// Only the SHA-256 of the secret is stored; the secret is shown once, when it is made.
type APIToken struct {
	ID        int64
	UserID    int64
	Name      string
	CreatedAt int64
	LastUsed  int64
	LastIP    string
}

const tokenCols = `id, user_id, name, created_at, last_used, last_ip`

func scanToken(row rowScanner) (APIToken, error) {
	var t APIToken
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.CreatedAt, &t.LastUsed, &t.LastIP)
	return t, err
}

// CreateAPIToken stores a token under the hash of its secret, unless the user already has
// maxTokens. It reports false when the limit is reached.
func (s *Store) CreateAPIToken(tokenHash string, t APIToken, maxTokens int) (int64, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`, t.UserID).Scan(&n); err != nil {
		return 0, false, err
	}
	if n >= maxTokens {
		return 0, false, nil
	}
	res, err := tx.Exec(`INSERT INTO api_tokens(user_id, token_hash, name, created_at) VALUES(?,?,?,?)`,
		t.UserID, tokenHash, t.Name, t.CreatedAt)
	if err != nil {
		return 0, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit()
}

// APITokenByHash finds the token whose secret hashes to tokenHash.
func (s *Store) APITokenByHash(tokenHash string) (APIToken, error) {
	t, err := scanToken(s.db.QueryRow(`SELECT `+tokenCols+` FROM api_tokens WHERE token_hash = ?`, tokenHash))
	return t, notFound(err)
}

// TouchAPIToken records a use, writing at most once per minGap seconds to spare the SD card.
func (s *Store) TouchAPIToken(id int64, ip string, minGap int64) error {
	now := s.Now()
	_, err := s.db.Exec(`UPDATE api_tokens SET last_used = ?, last_ip = ? WHERE id = ? AND last_used <= ?`, now, ip, id, now-minGap)
	return err
}

// ListAPITokens returns the user's tokens, newest first.
func (s *Store) ListAPITokens(userID int64) ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT `+tokenCols+` FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes one token and reports whether it existed.
func (s *Store) DeleteAPIToken(userID, id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM api_tokens WHERE user_id = ? AND id = ?`, userID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
