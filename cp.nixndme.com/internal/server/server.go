// Package server is the HTTP layer: owner dashboard, JSON API and public share pages.
package server

import (
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cpshare/internal/auth"
	"cpshare/internal/config"
	"cpshare/internal/live"
	"cpshare/internal/store"
	"cpshare/internal/vault"
)

const (
	sessionCookie   = "cps"
	sessionLifetime = 7 * 24 * time.Hour
	grantLifetime   = 30 * time.Minute // how long a password unlock lasts
	burnWindow      = 15 * time.Minute // how long a revealed burn-after-reading share stays readable
	staleUploadAge  = 6 * time.Hour
)

// Server holds every dependency of the HTTP handlers.
type Server struct {
	cfg      config.Config
	st       *store.Store
	vault    *vault.Vault
	log      *slog.Logger
	pages    map[string]*template.Template
	lockout  *auth.Lockout
	public   *auth.RateLimiter // per-IP limit on unauthenticated share routes
	unlock   *auth.Lockout     // per-IP lockout on share password guesses
	totp     *auth.TOTPVerifier
	kick     chan struct{} // asks the cleanup loop to run now
	uploadMu sync.Map      // blob id -> *sync.Mutex, serialises chunks of one upload
	live     *live.Hub     // Live Transfer meeting point (in memory)
	origin   string
}

// New builds a server. kick may be nil.
func New(cfg config.Config, st *store.Store, v *vault.Vault, log *slog.Logger, kick chan struct{}) (*Server, error) {
	pages, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: cfg, st: st, vault: v, log: log, pages: pages, kick: kick,
		lockout: auth.NewLockout(5, 15*time.Minute, 15*time.Minute),
		unlock:  auth.NewLockout(10, 15*time.Minute, 15*time.Minute),
		public:  auth.NewRateLimiter(120, 60),
		totp:    auth.NewTOTPVerifier(),
		live:    newLiveHub(),
		origin:  u.Scheme + "://" + u.Host,
	}, nil
}

// Handler returns the full route table wrapped in the security middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /static/chroma.css", s.chromaCSS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /{$}", s.requirePage(s.dashboardPage))
	mux.HandleFunc("GET /settings", s.requirePage(s.settingsPage))

	s.apiRoutes(mux)
	s.liveRoutes(mux)
	s.publicRoutes(mux)
	return s.secure(mux)
}

func (s *Server) apiRoutes(mux *http.ServeMux) {
	api := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.requireAPI(h)) }
	api("GET /api/shares", s.listShares)
	api("POST /api/shares", s.createShare)
	api("PATCH /api/shares/{slug}", s.updateShare)
	api("DELETE /api/shares/{slug}", s.deleteShare)
	api("POST /api/shares/{slug}/regenerate", s.regenerateShare)
	api("GET /api/shares/{slug}/log", s.shareLog)
	api("POST /api/uploads", s.startUpload)
	api("PUT /api/uploads/{id}/{chunk}", s.putChunk)
	api("DELETE /api/uploads/{id}", s.cancelUpload)
	api("GET /api/settings", s.getSettings)
	api("PUT /api/settings", s.putSettings)
	api("GET /api/usage", s.usage)
	api("POST /api/history/clear", s.clearHistory)
	api("POST /api/account/password", s.changePassword)
	api("POST /api/account/username", s.changeUsername)
	api("POST /api/account/totp/setup", s.totpSetup)
	api("POST /api/account/totp/enable", s.totpEnable)
	api("POST /api/account/totp/disable", s.totpDisable)
	api("POST /api/account/logout-others", s.logoutOthers)
}

func (s *Server) publicRoutes(mux *http.ServeMux) {
	pub := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.rateLimited(h)) }
	pub("GET /s/{slug}", s.viewShare)
	pub("POST /s/{slug}/unlock", s.unlockShare)
	pub("POST /s/{slug}/reveal", s.revealShare)
	pub("GET /r/{slug}", s.rawShare)
	pub("GET /d/{slug}/{file}", s.downloadFile)
	pub("GET /z/{slug}", s.zipShare)
	pub("GET /qr/{slug}", s.qrShare)
}

// secure adds security headers, rejects cross-site writes and recovers from panics.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; object-src 'none'; "+
			"frame-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "same-origin") // share URLs are secrets: never send them to other sites
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.cfg.SecureCookies {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "path", r.URL.Path, "err", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// sameOrigin accepts a write only when it comes from this site. Modern browsers say so in
// Sec-Fetch-Site; otherwise Origin (or Referer) must match. Cross-site CSRF is refused.
func (s *Server) sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "same-site", "cross-site":
		return false
	}
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true // non-browser client (curl); the API still requires the CSRF header
	}
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	return u.Scheme+"://"+u.Host == s.origin || strings.EqualFold(u.Host, r.Host)
}

func (s *Server) rateLimited(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentUser(r); !ok && !s.public.Allow(s.clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	}
}

// Kick asks the cleanup loop to run as soon as possible.
func (s *Server) Kick() {
	if s.kick == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// now is the server's clock; it follows the store's so tests can move time for everything.
func (s *Server) now() time.Time { return time.Unix(s.st.Now(), 0) }

func (s *Server) lockUpload(id string) func() {
	m, _ := s.uploadMu.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
