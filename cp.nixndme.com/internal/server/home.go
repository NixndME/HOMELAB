package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpshare/internal/auth"
	"cpshare/internal/ids"
)

// A discreet front door. Once the owner sets a secret word, strangers never see a login:
// "/" is an ordinary start page (Glance, proxied under /home) and /login answers like any
// unknown address. Typing the word into the start page's search box (it searches via /q)
// hands this browser a short-lived ticket that opens the login; every other search goes
// to a normal search engine. The word only hides the door: password and 2FA still apply.
const (
	loginWordKey    = "login_word" // argon2id hash of the normalized word; "" = not hidden
	ticketCookie    = "pref"       // deliberately bland
	ticketLifetime  = 10 * time.Minute
	maxTickets      = 200
	minLoginWord    = 8
	maxLoginWord    = 200
	homePrefix      = "/home"
	homeThemeCookie = "theme" // the start page's own theme choice; the only cookie it gets
	maxHomeHTML     = 4 << 20
)

// ---------- the secret word ----------

// normalizeWord makes the word forgiving to type on a phone: case and extra spaces ignored.
func normalizeWord(w string) string { return strings.Join(strings.Fields(strings.ToLower(w)), " ") }

// storedWord returns the word's hash, or "" when no word is set (the login is then open).
// A database error also counts as "not set": the owner must never be locked out.
func (s *Server) storedWord() string {
	all, err := s.st.Settings()
	if err != nil {
		s.log.Error("load login word: sign-in page shown openly until the database recovers", "err", err)
		return ""
	}
	return all[loginWordKey]
}

// loginHidden reports whether the login is hidden behind the secret word.
func (s *Server) loginHidden() bool { return s.storedWord() != "" }

// wordMatches checks a guess with the same slow hash as passwords. Checks run one at a
// time, so many at once cannot exhaust the Pi's memory (each takes 64 MB).
func (s *Server) wordMatches(guess string) bool {
	stored, g := s.storedWord(), normalizeWord(guess)
	if stored == "" || g == "" {
		return false
	}
	s.wordMu.Lock()
	defer s.wordMu.Unlock()
	return auth.VerifyPassword(stored, g)
}

// ---------- login tickets (in memory; a restart just means typing the word again) ----------

type ticketStore struct {
	mu sync.Mutex
	m  map[string]time.Time // token hash -> expiry
}

func newTicketStore() *ticketStore { return &ticketStore{m: map[string]time.Time{}} }

func (t *ticketStore) issue(now time.Time) string {
	token := ids.Token()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, exp := range t.m {
		if !now.Before(exp) {
			delete(t.m, k)
		}
	}
	if len(t.m) >= maxTickets { // only reachable by someone who knows the word
		t.m = map[string]time.Time{}
	}
	t.m[auth.HashToken(token)] = now.Add(ticketLifetime)
	return token
}

func (t *ticketStore) valid(token string, now time.Time) bool {
	if !ids.Valid(token, ids.TokenLength) {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.m[auth.HashToken(token)]
	return ok && now.Before(exp)
}

func (t *ticketStore) consume(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, auth.HashToken(token))
}

func (t *ticketStore) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m = map[string]time.Time{}
}

// loginAllowed says whether this request may see or use the login form.
func (s *Server) loginAllowed(r *http.Request) bool {
	if !s.loginHidden() {
		return true
	}
	c, err := r.Cookie(ticketCookie)
	return err == nil && s.tickets.valid(c.Value, s.now())
}

func (s *Server) consumeTicket(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(ticketCookie); err == nil {
		s.tickets.consume(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: ticketCookie, Value: "", Path: "/login", MaxAge: -1, HttpOnly: true, Secure: s.cfg.SecureCookies})
}

// entryPath is where a logged-out owner is sent: the login, or the start page once hidden.
func (s *Server) entryPath() string {
	if s.loginHidden() {
		return "/"
	}
	return "/login"
}

// ---------- routes ----------

func (s *Server) homeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.root)
	mux.HandleFunc("GET /q", s.rateLimited(s.search))
	mux.HandleFunc("GET /home", s.rateLimited(s.homeProxy))
	mux.HandleFunc("GET /home/", s.rateLimited(s.homeProxy))
	// The only write the start page makes: picking a theme. Same-origin only (see secure).
	mux.HandleFunc("POST /home/api/set-theme/{key}", s.rateLimited(func(w http.ResponseWriter, r *http.Request) {
		if !themeKey.MatchString(r.PathValue("key")) {
			http.NotFound(w, r)
			return
		}
		r.Body = http.NoBody
		s.homeProxy(w, r)
	}))
}

var themeKey = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// root is the dashboard for the owner, the login for others until a secret word is set,
// and the start page after that.
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if o, ok := s.currentUser(r); ok {
		s.touch(r, o)
		s.dashboardPage(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, o)))
		return
	}
	if !s.loginHidden() {
		target := "/login"
		if r.URL.Query().Get("idle") != "" {
			target += "?idle=1"
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	if s.home != nil {
		http.Redirect(w, r, homePrefix+"/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "start", map[string]any{})
}

// search sends the secret word to the login and everything else to the search engine.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	// The search (maybe the word) never travels on as a referrer, not even to /login.
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Guesses beyond the limits are simply searched, so trying words teaches nothing.
	if len(q) <= maxLoginWord && s.loginHidden() && s.wordGuess.Allow(s.clientIP(r)) &&
		s.wordGuessAll.Allow("all") && s.wordMatches(q) {
		if _, ok := s.currentUser(r); ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: ticketCookie, Value: s.tickets.issue(s.now()), Path: "/login", HttpOnly: true,
			Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: int(ticketLifetime.Seconds()),
		})
		s.log.Info("login opened by the secret word", "ip", s.clientIP(r))
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, strings.ReplaceAll(s.cfg.SearchURL, "{QUERY}", url.QueryEscape(q)), http.StatusSeeOther)
}

// ---------- start page proxy ----------

func (s *Server) homeProxy(w http.ResponseWriter, r *http.Request) {
	if s.home == nil {
		http.NotFound(w, r)
		return
	}
	// The proxy sets a CSP that fits the page. COOP is dropped here only: cp's own pages
	// keep it, so a window the start page opens onto cp is cut off from it.
	w.Header().Del("Content-Security-Policy")
	w.Header().Del("Cross-Origin-Opener-Policy")
	s.home.ServeHTTP(w, r)
}

func newHomeProxy(upstream string, s *Server) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			p := strings.TrimPrefix(pr.In.URL.Path, homePrefix)
			if p == "" {
				p = "/"
			}
			pr.Out.URL.Path, pr.Out.URL.RawPath = p, ""
			pr.SetURL(target)
			pr.SetXForwarded()
			// The start page never sees cp's session, tokens or tickets: only its theme.
			pr.Out.Header.Del("Cookie")
			if c, err := pr.In.Cookie(homeThemeCookie); err == nil {
				pr.Out.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
			}
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Accept-Encoding") // plain HTML so the CSP can be computed
		},
		ModifyResponse: func(resp *http.Response) error { return setHomeCSP(resp, s.origin) },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("start page unavailable", "err", err)
			if r.URL.Path == "/" { // r is the outgoing request: /home/ already stripped to /
				s.render(w, http.StatusOK, "start", map[string]any{}) // the page itself: plain search box
				return
			}
			http.Error(w, "unavailable", http.StatusBadGateway)
		},
	}, nil
}

var (
	inlineScript = regexp.MustCompile(`(?s)<script(\s[^>]*)?>(.*?)</script>`)
	headEnd      = regexp.MustCompile(`(?i)</head>`)
)

// homeCSP fences the start page in, although it shares cp's origin: scripts only from its
// own files plus the exact hashes of its head scripts (text from feeds can never run), and
// fetches only to /home/, so its code cannot read cp's pages or call cp's API. No frames,
// workers or form posts. Styles may be inline (it themes itself); images may come from feeds.
func homeCSP(origin string, scriptHashes []string) string {
	home := origin + homePrefix + "/"
	return "default-src " + home + "; script-src " + home + "static/" + strings.Join(scriptHashes, "") +
		"; style-src 'self' 'unsafe-inline'; img-src 'self' https: data:; connect-src " + home +
		"; frame-src 'none'; worker-src 'none'; form-action 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
}

// keepThemeCookie drops every cookie the start page sets except its theme choice, scoped
// to /home so it can never reach or shadow cp's own cookies.
func keepThemeCookie(resp *http.Response) {
	cookies := resp.Cookies()
	resp.Header.Del("Set-Cookie")
	for _, c := range cookies {
		if c.Name == homeThemeCookie {
			resp.Header.Add("Set-Cookie", (&http.Cookie{Name: c.Name, Value: c.Value, Path: homePrefix + "/",
				MaxAge: c.MaxAge, Expires: c.Expires, HttpOnly: true, SameSite: http.SameSiteLaxMode}).String())
		}
	}
}

func setHomeCSP(resp *http.Response, origin string) error {
	keepThemeCookie(resp)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		resp.Header.Set("Content-Security-Policy", homeCSP(origin, nil))
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHomeHTML+1))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > maxHomeHTML {
		return io.ErrShortBuffer
	}
	// Only the page's own head scripts are trusted. Widget responses (/api/…) carry feed text
	// and get no script allowance at all, even if opened directly.
	var head []byte
	if loc := headEnd.FindIndex(body); loc != nil {
		head = body[:loc[0]]
	}
	if resp.Request != nil && strings.HasPrefix(resp.Request.URL.Path, "/api/") {
		head = nil
	}
	var hashes []string
	for _, m := range inlineScript.FindAllSubmatch(head, -1) {
		if bytes.Contains(bytes.ToLower(m[1]), []byte("src=")) {
			continue
		}
		sum := sha256.Sum256(m[2])
		hashes = append(hashes, " 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	resp.Header.Set("Content-Security-Policy", homeCSP(origin, hashes))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Set("Cache-Control", "no-store")
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}

// ---------- Settings: set, change or clear the secret word ----------

func (s *Server) getLoginWord(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"set": s.loginHidden(), "start_page": s.home != nil, "min": minLoginWord})
}

// putLoginWord sets the word (or clears it with an empty word) after the password is
// re-entered. Clearing makes /login reachable again. Any open tickets end either way.
func (s *Server) putLoginWord(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Word     string `json:"word"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	word := normalizeWord(req.Word)
	if word != "" && (len([]rune(word)) < minLoginWord || len(word) > maxLoginWord) {
		apiError(w, http.StatusBadRequest, "the secret word must be "+strconv.Itoa(minLoginWord)+" to "+strconv.Itoa(maxLoginWord)+" characters")
		return
	}
	if !s.checkPassword(w, r, req.Password) {
		return
	}
	value := ""
	if word != "" {
		var err error
		if value, err = auth.HashPassword(word); err != nil {
			s.internal(w, "hash login word", err)
			return
		}
	}
	if err := s.st.PutSettings(map[string]string{loginWordKey: value}); err != nil {
		s.internal(w, "save login word", err)
		return
	}
	s.tickets.clear()
	s.log.Info("login word changed", "set", word != "")
	writeJSON(w, http.StatusOK, map[string]bool{"set": word != ""})
}
