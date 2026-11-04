package store

import (
	"database/sql"
	"strconv"
)

// User is the single login account.
type User struct {
	ID          int64
	Username    string
	PassHash    string
	TOTPSecret  string
	TOTPEnabled bool
}

// Session is a logged-in browser.
type Session struct {
	ID        int64 // public handle for listing and ending one session; never the token
	UserID    int64
	CSRF      string
	CreatedAt int64
	ExpiresAt int64
	LastSeen  int64 // last request the owner made (background polls excluded)
	IP        string
	UA        string
}

const sessionCols = `rowid, user_id, csrf, created_at, expires_at, last_seen, ip, ua`

// liveSession is the condition a usable session meets: not past its absolute expiry and
// used after idleSince. It takes (now, idleSince).
const liveSession = `expires_at > ? AND last_seen > ?`

type rowScanner interface{ Scan(dest ...any) error }

func scanSession(row rowScanner) (Session, error) {
	var sess Session
	err := row.Scan(&sess.ID, &sess.UserID, &sess.CSRF, &sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeen, &sess.IP, &sess.UA)
	return sess, err
}

// CountUsers returns how many accounts exist (0 on first boot).
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts the account. Called once, on first boot.
func (s *Store) CreateUser(username, passHash string) error {
	_, err := s.db.Exec(`INSERT INTO users(username, pass_hash, updated_at) VALUES(?,?,?)`,
		username, passHash, s.Now())
	return err
}

const userCols = `id, username, pass_hash, totp_secret, totp_enabled`

func scanUser(row *sql.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PassHash, &u.TOTPSecret, &u.TOTPEnabled)
	return u, notFound(err)
}

// UserByName finds the account by username.
func (s *Store) UserByName(username string) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

// UserByID finds the account by id.
func (s *Store) UserByID(id int64) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// SetPassword replaces the password hash.
func (s *Store) SetPassword(id int64, passHash string) error {
	_, err := s.db.Exec(`UPDATE users SET pass_hash = ?, updated_at = ? WHERE id = ?`, passHash, s.Now(), id)
	return err
}

// SetUsername renames the account.
func (s *Store) SetUsername(id int64, username string) error {
	_, err := s.db.Exec(`UPDATE users SET username = ?, updated_at = ? WHERE id = ?`, username, s.Now(), id)
	return err
}

// SetTOTP stores the 2FA secret and whether it is active.
func (s *Store) SetTOTP(id int64, secret string, enabled bool) error {
	_, err := s.db.Exec(`UPDATE users SET totp_secret = ?, totp_enabled = ?, updated_at = ? WHERE id = ?`,
		secret, enabled, s.Now(), id)
	return err
}

// CreateSession stores a session under the hash of its token, then keeps only the user's
// maxSessions most recently used live sessions (the new one included); expired and idle
// ones go too. It returns how many other sessions were removed.
func (s *Store) CreateSession(tokenHash string, sess Session, maxSessions int, idleSince int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO sessions(token_hash, user_id, csrf, created_at, expires_at, last_seen, ip, ua) VALUES(?,?,?,?,?,?,?,?)`,
		tokenHash, sess.UserID, sess.CSRF, sess.CreatedAt, sess.ExpiresAt, sess.CreatedAt, sess.IP, sess.UA); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ? AND rowid NOT IN (
		SELECT rowid FROM sessions WHERE user_id = ? AND token_hash <> ? AND `+liveSession+`
		ORDER BY last_seen DESC, created_at DESC LIMIT ?)`,
		sess.UserID, tokenHash, sess.UserID, tokenHash, s.Now(), idleSince, maxSessions-1)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// SessionByHash returns a session that is neither expired nor idle since idleSince.
func (s *Store) SessionByHash(tokenHash string, idleSince int64) (Session, error) {
	sess, err := scanSession(s.db.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE token_hash = ? AND `+liveSession,
		tokenHash, s.Now(), idleSince))
	return sess, notFound(err)
}

// TouchSession records activity on a session, writing at most once per minGap seconds to
// spare the SD card.
func (s *Store) TouchSession(tokenHash string, minGap int64) error {
	now := s.Now()
	_, err := s.db.Exec(`UPDATE sessions SET last_seen = ? WHERE token_hash = ? AND last_seen <= ?`, now, tokenHash, now-minGap)
	return err
}

// ListSessions returns the user's live sessions, most recently used first.
func (s *Store) ListSessions(userID, idleSince int64) ([]Session, error) {
	rows, err := s.db.Query(`SELECT `+sessionCols+` FROM sessions WHERE user_id = ? AND `+liveSession+
		` ORDER BY last_seen DESC, created_at DESC`, userID, s.Now(), idleSince)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// DeleteSessionByID logs out the user's session with that id and reports whether it existed.
func (s *Store) DeleteSessionByID(userID, id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND rowid = ?`, userID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteSession logs one browser out.
func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteOtherSessions logs out every browser except the one holding keepHash.
func (s *Store) DeleteOtherSessions(userID int64, keepHash string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepHash)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Settings returns every stored setting.
func (s *Store) Settings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// PutSettings upserts settings in one transaction.
func (s *Store) PutSettings(values map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range values {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FormatInt is a small helper for storing integers as setting values.
func FormatInt(v int64) string { return strconv.FormatInt(v, 10) }

// ResetCredentials sets a new password for every account, turns off 2FA and ends all
// sessions. Used by the `cp reset-password` recovery command.
func (s *Store) ResetCredentials(passHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET pass_hash = ?, totp_secret = '', totp_enabled = 0, updated_at = ?`, passHash, s.Now()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM api_tokens`); err != nil {
		return err
	}
	return tx.Commit()
}
