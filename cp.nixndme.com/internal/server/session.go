package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"time"

	"cpshare/internal/auth"
	"cpshare/internal/ids"
	"cpshare/internal/store"
)

// backgroundHeader marks requests the page makes on its own (list refresh, idle checks).
// They are answered normally but do not count as activity, so an open but unattended tab
// still logs out after the idle time.
const backgroundHeader = "X-CP-Background"

type ctxKey struct{}

// owner is the authenticated session attached to a request.
type owner struct {
	UserID    int64
	SessionID int64
	CSRF      string
	TokenHash string
	LastSeen  int64
}

func ownerFrom(ctx context.Context) owner {
	o, _ := ctx.Value(ctxKey{}).(owner)
	return o
}

// sessionIdle is the configured inactivity limit.
func (s *Server) sessionIdle() time.Duration {
	c, err := LoadSettings(s.st)
	if err != nil {
		s.log.Error("load settings", "err", err)
		c = defaultSettings()
	}
	return time.Duration(c.SessionIdle) * time.Second
}

// idleSince is the oldest activity time a session may have and still be live.
func (s *Server) idleSince() int64 {
	return s.now().Add(-s.sessionIdle()).Unix()
}

// currentUser resolves the session cookie, if any. Expired and idle sessions do not count.
func (s *Server) currentUser(r *http.Request) (owner, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || !ids.Valid(c.Value, ids.TokenLength) {
		return owner{}, false
	}
	hash := auth.HashToken(c.Value)
	sess, err := s.st.SessionByHash(hash, s.idleSince())
	if err != nil {
		return owner{}, false
	}
	return owner{UserID: sess.UserID, SessionID: sess.ID, CSRF: sess.CSRF, TokenHash: hash, LastSeen: sess.LastSeen}, true
}

// touch records the request as activity unless the page marked it as background.
func (s *Server) touch(r *http.Request, o owner) {
	if r.Header.Get(backgroundHeader) != "" {
		return
	}
	if err := s.st.TouchSession(o.TokenHash, int64(touchGap.Seconds())); err != nil {
		s.log.Error("touch session", "err", err)
	}
}

func (s *Server) requirePage(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o, ok := s.currentUser(r)
		if !ok {
			http.Redirect(w, r, s.entryPath(), http.StatusSeeOther)
			return
		}
		s.touch(r, o)
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, o)))
	}
}

// requireAPI demands a session plus the session's CSRF token in X-CSRF-Token on writes.
func (s *Server) requireAPI(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o, ok := s.currentUser(r)
		if !ok {
			if s.loginHidden() { // say nothing about an API to strangers
				http.NotFound(w, r)
				return
			}
			apiError(w, http.StatusUnauthorized, "login required")
			return
		}
		if r.Method != http.MethodGet && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(o.CSRF)) != 1 {
			apiError(w, http.StatusForbidden, "missing or wrong CSRF token")
			return
		}
		s.touch(r, o)
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, o)))
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.loginAllowed(r) { // hidden behind the secret word: look like any unknown page
		http.NotFound(w, r)
		return
	}
	data := map[string]any{}
	if r.URL.Query().Get("idle") != "" {
		data["Notice"] = "You were logged out: no activity for a while, or this login was ended from another browser."
	}
	s.render(w, http.StatusOK, "login", data)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.loginAllowed(r) {
		http.NotFound(w, r)
		return
	}
	ip := s.clientIP(r)
	fail := func(status int, msg string) {
		s.render(w, status, "login", map[string]any{"Error": msg, "Username": r.PostFormValue("username")})
	}
	if locked, left := s.lockout.Locked(ip); locked {
		fail(http.StatusTooManyRequests, fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", int(left.Minutes())+1))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	username, password, code := r.PostFormValue("username"), r.PostFormValue("password"), r.PostFormValue("code")
	u, err := s.st.UserByName(username)
	ok := false
	if err == nil {
		ok = auth.VerifyPassword(u.PassHash, password) && (!u.TOTPEnabled || s.totp.Verify(u.TOTPSecret, code))
	} else {
		auth.VerifyDummy(password)
	}
	if !ok {
		s.log.Warn("login failed", "ip", ip)
		if s.lockout.Fail(ip) {
			fail(http.StatusTooManyRequests, "Too many failed attempts. Try again in 15 minutes.")
			return
		}
		fail(http.StatusUnauthorized, "Wrong username, password or code.")
		return
	}
	s.lockout.Succeed(ip)
	if err := s.startSession(w, r, u); err != nil {
		s.log.Error("create session", "err", err)
		fail(http.StatusInternalServerError, "Could not log in, please retry.")
		return
	}
	s.consumeTicket(w, r)
	s.log.Info("login", "ip", ip)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// startSession logs this browser in. Beyond maxSessions, the least recently used other
// login is ended, so a new login always works and old devices drop off.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	token := ids.Token()
	now := s.now()
	evicted, err := s.st.CreateSession(auth.HashToken(token), store.Session{
		UserID: u.ID, CSRF: ids.Token(), CreatedAt: now.Unix(),
		ExpiresAt: now.Add(sessionLifetime).Unix(), IP: s.clientIP(r), UA: userAgent(r),
	}, maxSessions, s.idleSince())
	if err != nil {
		return err
	}
	if evicted > 0 {
		s.log.Info("sessions ended at login", "count", evicted)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionLifetime.Seconds()),
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.SecureCookies})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if o, ok := s.currentUser(r); ok {
		if err := s.st.DeleteSession(o.TokenHash); err != nil {
			s.log.Error("delete session", "err", err)
		}
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, s.entryPath(), http.StatusSeeOther)
}
