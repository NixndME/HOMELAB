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
	UserID    int64
	CSRF      string
	CreatedAt int64
	ExpiresAt int64
	IP        string
	UA        string
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

// CreateSession stores a session under the hash of its token.
func (s *Store) CreateSession(tokenHash string, sess Session) error {
	_, err := s.db.Exec(`INSERT INTO sessions(token_hash, user_id, csrf, created_at, expires_at, ip, ua) VALUES(?,?,?,?,?,?,?)`,
		tokenHash, sess.UserID, sess.CSRF, sess.CreatedAt, sess.ExpiresAt, sess.IP, sess.UA)
	return err
}

// SessionByHash returns an unexpired session.
func (s *Store) SessionByHash(tokenHash string) (Session, error) {
	var sess Session
	err := s.db.QueryRow(`SELECT user_id, csrf, created_at, expires_at, ip, ua FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, s.Now()).Scan(&sess.UserID, &sess.CSRF, &sess.CreatedAt, &sess.ExpiresAt, &sess.IP, &sess.UA)
	return sess, notFound(err)
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

// CountSessions returns the number of live sessions.
func (s *Store) CountSessions(userID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ?`, userID, s.Now()).Scan(&n)
	return n, err
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
	return tx.Commit()
}
