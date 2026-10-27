package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"

	"cpshare/internal/auth"
	"cpshare/internal/ids"
	"cpshare/internal/store"
)

type ctxKey struct{}

// owner is the authenticated session attached to a request.
type owner struct {
	UserID    int64
	CSRF      string
	TokenHash string
}

func ownerFrom(ctx context.Context) owner {
	o, _ := ctx.Value(ctxKey{}).(owner)
	return o
}

// currentUser resolves the session cookie, if any.
func (s *Server) currentUser(r *http.Request) (owner, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || !ids.Valid(c.Value, ids.TokenLength) {
		return owner{}, false
	}
	hash := auth.HashToken(c.Value)
	sess, err := s.st.SessionByHash(hash)
	if err != nil {
		return owner{}, false
	}
	return owner{UserID: sess.UserID, CSRF: sess.CSRF, TokenHash: hash}, true
}

func (s *Server) requirePage(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o, ok := s.currentUser(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, o)))
	}
}

// requireAPI demands a session plus the session's CSRF token in X-CSRF-Token on writes.
func (s *Server) requireAPI(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o, ok := s.currentUser(r)
		if !ok {
			apiError(w, http.StatusUnauthorized, "login required")
			return
		}
		if r.Method != http.MethodGet && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(o.CSRF)) != 1 {
			apiError(w, http.StatusForbidden, "missing or wrong CSRF token")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, o)))
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", map[string]any{})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
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
	s.log.Info("login", "ip", ip)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	token := ids.Token()
	now := s.now()
	err := s.st.CreateSession(auth.HashToken(token), store.Session{
		UserID: u.ID, CSRF: ids.Token(), CreatedAt: now.Unix(),
		ExpiresAt: now.Add(sessionLifetime).Unix(), IP: s.clientIP(r), UA: userAgent(r),
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionLifetime.Seconds()),
	})
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if o, ok := s.currentUser(r); ok {
		if err := s.st.DeleteSession(o.TokenHash); err != nil {
			s.log.Error("delete session", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.SecureCookies})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
