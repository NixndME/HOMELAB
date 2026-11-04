package server

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/skip2/go-qrcode"

	"cpshare/internal/auth"
	"cpshare/internal/config"
)

const totpIssuer = "cp"

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{3,64}$`)

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	c, err := LoadSettings(s.st)
	if err != nil {
		s.internal(w, "load settings", err)
		return
	}
	u, err := s.st.UserByID(ownerFrom(r.Context()).UserID)
	if err != nil {
		s.internal(w, "load user", err)
		return
	}
	live, err := s.st.ListSessions(u.ID, s.idleSince())
	if err != nil {
		s.internal(w, "list sessions", err)
		return
	}
	sessions := len(live)
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": c, "username": u.Username, "totp_enabled": u.TOTPEnabled, "sessions": sessions,
		"limits": map[string]any{"max_file_bytes": s.cfg.MaxFileBytes, "storage_bytes": s.cfg.StorageBytes,
			"max_files": s.cfg.MaxFiles, "chunk_bytes": s.cfg.ChunkBytes},
	})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var c Settings
	if err := readJSON(w, r, &c); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := SaveSettings(s.st, c); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Kick() // a new cleanup interval or history window takes effect now
	writeJSON(w, http.StatusOK, map[string]any{"settings": c})
}

func (s *Server) usage(w http.ResponseWriter, _ *http.Request) {
	used, err := s.st.StoredBytes()
	if err != nil {
		s.internal(w, "usage", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"used_bytes": used, "cap_bytes": s.cfg.StorageBytes})
}

func (s *Server) clearHistory(w http.ResponseWriter, _ *http.Request) {
	n, err := s.st.ClearHistory()
	if err != nil {
		s.internal(w, "clear history", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"removed": n})
}

// checkPassword re-authenticates the owner for sensitive account changes.
func (s *Server) checkPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	ip := s.clientIP(r)
	if locked, _ := s.lockout.Locked(ip); locked {
		apiError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return false
	}
	u, err := s.st.UserByID(ownerFrom(r.Context()).UserID)
	if err != nil || !auth.VerifyPassword(u.PassHash, password) {
		s.lockout.Fail(ip)
		apiError(w, http.StatusForbidden, "current password is wrong")
		return false
	}
	s.lockout.Succeed(ip)
	return true
}

func validNewPassword(p string) bool {
	n := utf8.RuneCountInString(p)
	return n >= config.MinPasswordLength() && n <= 256
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validNewPassword(req.New) {
		apiError(w, http.StatusBadRequest, "new password must be "+strconv.Itoa(config.MinPasswordLength())+" to 256 characters")
		return
	}
	if !s.checkPassword(w, r, req.Current) {
		return
	}
	hash, err := auth.HashPassword(req.New)
	if err != nil {
		s.internal(w, "hash password", err)
		return
	}
	o := ownerFrom(r.Context())
	if err := s.st.SetPassword(o.UserID, hash); err != nil {
		s.internal(w, "set password", err)
		return
	}
	n, _ := s.st.DeleteOtherSessions(o.UserID, o.TokenHash) // a changed password ends every other login
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "other_sessions_ended": n})
}

func (s *Server) changeUsername(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Username string `json:"username"`
	}
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !usernameRe.MatchString(req.Username) {
		apiError(w, http.StatusBadRequest, "username must be 3 to 64 of A-Z a-z 0-9 . _ @ -")
		return
	}
	if !s.checkPassword(w, r, req.Password) {
		return
	}
	if err := s.st.SetUsername(ownerFrom(r.Context()).UserID, req.Username); err != nil {
		s.internal(w, "set username", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// totpSetup creates a fresh, not yet enabled secret and returns it with a QR code.
func (s *Server) totpSetup(w http.ResponseWriter, r *http.Request) {
	u, err := s.st.UserByID(ownerFrom(r.Context()).UserID)
	if err != nil {
		s.internal(w, "load user", err)
		return
	}
	if u.TOTPEnabled {
		apiError(w, http.StatusConflict, "2FA is already on; turn it off first")
		return
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		s.internal(w, "totp secret", err)
		return
	}
	if err := s.st.SetTOTP(u.ID, secret, false); err != nil {
		s.internal(w, "store totp", err)
		return
	}
	uri := auth.TOTPURI(totpIssuer, u.Username, secret)
	png, err := qrcode.Encode(uri, qrcode.Medium, 256)
	if err != nil {
		s.internal(w, "totp qr", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"secret": secret, "uri": uri, "qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
}

func (s *Server) totpEnable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.st.UserByID(ownerFrom(r.Context()).UserID)
	if err != nil {
		s.internal(w, "load user", err)
		return
	}
	if u.TOTPSecret == "" || !s.totp.Verify(u.TOTPSecret, req.Code) {
		apiError(w, http.StatusBadRequest, "that code is not valid; check your phone's clock and retry")
		return
	}
	if err := s.st.SetTOTP(u.ID, u.TOTPSecret, true); err != nil {
		s.internal(w, "enable totp", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.checkPassword(w, r, req.Password) {
		return
	}
	if err := s.st.SetTOTP(ownerFrom(r.Context()).UserID, "", false); err != nil {
		s.internal(w, "disable totp", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) logoutOthers(w http.ResponseWriter, r *http.Request) {
	o := ownerFrom(r.Context())
	n, err := s.st.DeleteOtherSessions(o.UserID, o.TokenHash)
	if err != nil {
		s.internal(w, "logout others", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"ended": n})
}

type sessionJSON struct {
	ID        int64  `json:"id"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	ExpiresAt int64  `json:"expires_at"`
	IP        string `json:"ip"`
	UA        string `json:"ua"`
	Current   bool   `json:"current"`
}

// listSessions shows every live login so the owner can end them one by one.
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	o := ownerFrom(r.Context())
	list, err := s.st.ListSessions(o.UserID, s.idleSince())
	if err != nil {
		s.internal(w, "list sessions", err)
		return
	}
	out := make([]sessionJSON, 0, len(list))
	for _, sess := range list {
		out = append(out, sessionJSON{ID: sess.ID, CreatedAt: sess.CreatedAt, LastSeen: sess.LastSeen,
			ExpiresAt: sess.ExpiresAt, IP: sess.IP, UA: sess.UA, Current: sess.ID == o.SessionID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "max": maxSessions,
		"idle_seconds": int64(s.sessionIdle().Seconds())})
}

// endSession logs out one login. Ending the current one also clears this browser's cookie.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusBadRequest, "bad session id")
		return
	}
	o := ownerFrom(r.Context())
	found, err := s.st.DeleteSessionByID(o.UserID, id)
	if err != nil {
		s.internal(w, "end session", err)
		return
	}
	if !found {
		apiError(w, http.StatusNotFound, "that login has already ended")
		return
	}
	current := id == o.SessionID
	if current {
		s.clearSessionCookie(w)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "current": current})
}

// sessionStatus tells the page how long until this login ends from inactivity. Pages call it
// as a background request to check, and as a normal request to report activity.
func (s *Server) sessionStatus(w http.ResponseWriter, r *http.Request) {
	o := ownerFrom(r.Context())
	idle := s.sessionIdle()
	now := s.now().Unix()
	last := o.LastSeen
	if r.Header.Get(backgroundHeader) == "" && now-last >= int64(touchGap.Seconds()) {
		last = now // touch just recorded this request
	}
	left := max(last+int64(idle.Seconds())-now, 0)
	writeJSON(w, http.StatusOK, map[string]int64{"idle_seconds": int64(idle.Seconds()), "idle_left": left})
}

// internal logs the detail server-side and returns a generic message to the client.
func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	apiError(w, http.StatusInternalServerError, "internal error")
}
