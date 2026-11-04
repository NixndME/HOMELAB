package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cpshare/internal/auth"
	"cpshare/internal/ids"
	"cpshare/internal/store"
)

// API tokens let scripts create shares (the cpup command, curl) without a browser login.
// A token can only upload and create shares: it cannot list, read, change or delete them,
// and cannot touch settings or the account. Requests carry it as "Authorization: Bearer cpt_…".
// A browser never sends that header on its own, so cross-site requests cannot use it and
// token requests need no CSRF token.
const (
	tokenPrefix   = "cpt_"
	maxAPITokens  = 10
	maxTokenName  = 60
	tokenTouchGap = 60 // seconds between "last used" writes
)

// newAPISecret returns a fresh token secret: the prefix plus 43 base62 characters (~256 bits).
func newAPISecret() string { return tokenPrefix + ids.Token() }

// bearerSecret returns the token in an Authorization header, if it is well formed.
func bearerSecret(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, secret, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	secret = strings.TrimSpace(secret)
	rest, ok := strings.CutPrefix(secret, tokenPrefix)
	return secret, ok && ids.Valid(rest, ids.TokenLength)
}

// requireUploader accepts either a browser session (like requireAPI) or an API token.
// It guards only the routes that upload and create shares.
func (s *Server) requireUploader(h http.HandlerFunc) http.HandlerFunc {
	session := s.requireAPI(h)
	return func(w http.ResponseWriter, r *http.Request) {
		scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") { // no token (or a proxy's own header): browser path
			session(w, r)
			return
		}
		ip := s.clientIP(r)
		if locked, _ := s.tokenFail.Locked(ip); locked {
			w.Header().Set("Retry-After", "900")
			apiError(w, http.StatusTooManyRequests, "too many bad tokens, try again later")
			return
		}
		t, ok := s.lookupToken(r)
		if !ok {
			s.log.Warn("api token refused", "ip", ip)
			s.tokenFail.Fail(ip)
			apiError(w, http.StatusUnauthorized, "API token is wrong or was revoked")
			return
		}
		s.tokenFail.Succeed(ip)
		if err := s.st.TouchAPIToken(t.ID, ip, tokenTouchGap); err != nil {
			s.log.Error("touch api token", "err", err)
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, owner{UserID: t.UserID})))
	}
}

func (s *Server) lookupToken(r *http.Request) (store.APIToken, bool) {
	secret, ok := bearerSecret(r)
	if !ok {
		return store.APIToken{}, false
	}
	t, err := s.st.APITokenByHash(auth.HashToken(secret))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("load api token", "err", err)
		}
		return store.APIToken{}, false
	}
	return t, true
}

type tokenJSON struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used"`
	LastIP    string `json:"last_ip"`
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListAPITokens(ownerFrom(r.Context()).UserID)
	if err != nil {
		s.internal(w, "list api tokens", err)
		return
	}
	out := make([]tokenJSON, 0, len(list))
	for _, t := range list {
		out = append(out, tokenJSON{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, LastUsed: t.LastUsed, LastIP: t.LastIP})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out, "max": maxAPITokens, "base_url": s.cfg.BaseURL})
}

// createToken makes a token after the owner re-enters the password. The secret is returned
// once and never again.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := cleanTokenName(req.Name)
	if name == "" {
		apiError(w, http.StatusBadRequest, "give the token a name, e.g. the machine it is for")
		return
	}
	if !s.checkPassword(w, r, req.Password) {
		return
	}
	secret := newAPISecret()
	o := ownerFrom(r.Context())
	id, ok, err := s.st.CreateAPIToken(auth.HashToken(secret), store.APIToken{UserID: o.UserID, Name: name, CreatedAt: s.st.Now()}, maxAPITokens)
	if err != nil {
		s.internal(w, "create api token", err)
		return
	}
	if !ok {
		apiError(w, http.StatusConflict, "at most "+strconv.Itoa(maxAPITokens)+" tokens; revoke one first")
		return
	}
	s.log.Info("api token created", "id", id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": name, "token": secret})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusBadRequest, "bad token id")
		return
	}
	found, err := s.st.DeleteAPIToken(ownerFrom(r.Context()).UserID, id)
	if err != nil {
		s.internal(w, "delete api token", err)
		return
	}
	if !found {
		apiError(w, http.StatusNotFound, "that token is already revoked")
		return
	}
	s.log.Info("api token revoked", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// cleanTokenName keeps a short, printable label.
func cleanTokenName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	return truncate(name, maxTokenName)
}
