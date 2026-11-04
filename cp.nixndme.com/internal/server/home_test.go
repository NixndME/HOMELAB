package server

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cpshare/internal/config"
)

const secretWord = "Blue Mango Tram"

func (e *env) setWord(word string) result {
	e.t.Helper()
	return e.api("PUT", "/api/account/login-word", map[string]any{"word": word, "password": testPass})
}

// search types q into the start page's search box with client c.
func (e *env) search(c *http.Client, q string) result {
	e.t.Helper()
	return e.get(c, "/q?q="+url.QueryEscape(q))
}

func loggedIn(e *env, c *http.Client) bool { return e.get(c, "/api/session").status == http.StatusOK }

func TestLoginStaysOpenUntilAWordIsSet(t *testing.T) {
	e := newEnv(t)
	anon := e.client()
	if r := e.get(anon, "/"); r.status != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Fatalf("/ before a word: %d %s", r.status, r.header.Get("Location"))
	}
	if r := e.get(anon, "/?idle=1"); r.header.Get("Location") != "/login?idle=1" {
		t.Fatalf("idle notice lost: %s", r.header.Get("Location"))
	}
	if r := e.get(anon, "/login"); r.status != http.StatusOK {
		t.Fatalf("/login before a word: %d", r.status)
	}
	if r := e.search(anon, "kubernetes 1.36"); r.status != http.StatusSeeOther || r.header.Get("Location") != "https://search.example/?q=kubernetes+1.36" {
		t.Fatalf("search: %d %s", r.status, r.header.Get("Location"))
	}
}

func TestSettingTheWordNeedsPasswordAndLength(t *testing.T) {
	e := newEnv(t)
	if r := e.api("PUT", "/api/account/login-word", map[string]any{"word": secretWord, "password": "wrong"}); r.status != http.StatusForbidden {
		t.Fatalf("wrong password: %d", r.status)
	}
	if r := e.setWord("abc"); r.status != http.StatusBadRequest {
		t.Fatalf("3-letter word: %d", r.status)
	}
	if r := e.get(e.client(), "/api/account/login-word"); r.status != http.StatusUnauthorized {
		t.Fatalf("status for a stranger: %d", r.status)
	}
	if r := e.setWord(secretWord); r.status != http.StatusOK {
		t.Fatalf("set word: %d %s", r.status, r.body)
	}
	if all, _ := e.st.Settings(); strings.Contains(strings.ToLower(all[loginWordKey]), "mango") {
		t.Fatal("word stored in plain text")
	}
}

func TestHiddenLoginLooksLikeAnyMissingPage(t *testing.T) {
	e := newEnv(t)
	e.setWord(secretWord)
	anon := e.client()
	missing := e.get(anon, "/no-such-page")
	for _, path := range []string{"/login", "/login?idle=1"} {
		r := e.get(anon, path)
		if r.status != missing.status || r.body != missing.body {
			t.Errorf("GET %s: %d %q, want the same as a missing page", path, r.status, r.body)
		}
	}
	// Even the right password does nothing without the word first.
	if r := e.login(anon, testUser, testPass, ""); r.StatusCode != http.StatusNotFound || loggedIn(e, anon) {
		t.Fatalf("login without the word: %d", r.StatusCode)
	}
	start := e.get(anon, "/")
	if start.status != http.StatusOK || !strings.Contains(start.body, `action="/q"`) {
		t.Fatalf("start page: %d %.200s", start.status, start.body)
	}
	if r := e.get(anon, "/settings"); r.header.Get("Location") != "/" {
		t.Fatalf("/settings for a stranger goes to %q, want /", r.header.Get("Location"))
	}
}

func TestSecretWordOpensTheLoginOnce(t *testing.T) {
	e := newEnv(t)
	e.setWord(secretWord)
	c := e.client()
	if r := e.search(c, "blue mango"); !strings.HasPrefix(r.header.Get("Location"), "https://search.example/") {
		t.Fatalf("near miss: %s", r.header.Get("Location"))
	}
	r := e.search(c, "  BLUE   mango tram ")
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Fatalf("secret word: %d %s", r.status, r.header.Get("Location"))
	}
	if !strings.Contains(r.header.Get("Set-Cookie"), "Path=/login") {
		t.Fatalf("ticket cookie: %q", r.header.Get("Set-Cookie"))
	}
	ticket := strings.TrimPrefix(strings.Split(r.header.Get("Set-Cookie"), ";")[0], ticketCookie+"=")
	if p := e.get(c, "/login"); p.status != http.StatusOK || !strings.Contains(p.body, "Sign in") {
		t.Fatalf("login page with ticket: %d", p.status)
	}
	if resp := e.login(c, testUser, testPass, ""); resp.StatusCode != http.StatusSeeOther || !loggedIn(e, c) {
		t.Fatalf("login with ticket: %d", resp.StatusCode)
	}
	// The ticket is used up: replaying it opens nothing.
	if r := e.do(e.client(), "GET", "/login", nil, map[string]string{"Cookie": ticketCookie + "=" + ticket}); r.status != http.StatusNotFound {
		t.Fatalf("replayed ticket: %d", r.status)
	}
	// Logging out lands on the start page, not a login.
	if out := e.do(c, "POST", "/logout", nil, nil); out.header.Get("Location") != "/" {
		t.Fatalf("logout goes to %q", out.header.Get("Location"))
	}
}

func TestTicketExpires(t *testing.T) {
	e := newEnv(t)
	e.setWord(secretWord)
	c := e.client()
	e.search(c, secretWord)
	e.clock = e.clock.Add(ticketLifetime + time.Second)
	if r := e.get(c, "/login"); r.status != http.StatusNotFound {
		t.Fatalf("login after the ticket expired: %d", r.status)
	}
}

func TestWordGuessesAreRateLimitedSilently(t *testing.T) {
	e := newEnv(t)
	e.setWord(secretWord)
	c := e.client()
	for i := 0; i < 10; i++ {
		e.search(c, "guess number "+strconvFormat(int64(i)))
	}
	if r := e.search(c, secretWord); !strings.HasPrefix(r.header.Get("Location"), "https://search.example/") {
		t.Fatalf("right word after 10 guesses: %s, want a plain search", r.header.Get("Location"))
	}
}

func TestTurningTheWordOffReopensLogin(t *testing.T) {
	e := newEnv(t)
	e.setWord(secretWord)
	if r := e.setWord(""); r.status != http.StatusOK {
		t.Fatalf("clear: %d %s", r.status, r.body)
	}
	if r := e.get(e.client(), "/login"); r.status != http.StatusOK {
		t.Fatalf("/login after turning off: %d", r.status)
	}
}

func TestResetPasswordAlsoUnhidesTheLogin(t *testing.T) {
	e := newEnv(t)
	e.setWord(secretWord)
	if err := e.st.ResetCredentials("unused-hash"); err != nil {
		t.Fatal(err)
	}
	if r := e.get(e.client(), "/login"); r.status != http.StatusOK {
		t.Fatalf("/login after reset-password: %d, want reachable", r.status)
	}
}

func TestOwnerTypingTheWordJustGoesHome(t *testing.T) {
	e := newEnv(t)
	e.setWord(secretWord)
	if r := e.search(e.owner, secretWord); r.header.Get("Location") != "/" {
		t.Fatalf("owner: %s", r.header.Get("Location"))
	}
	if r := e.get(e.owner, "/"); r.status != http.StatusOK || !strings.Contains(r.body, "New share") {
		t.Fatalf("owner dashboard at /: %d", r.status)
	}
}

const fakeStartHead = `var pageData = {theme: "x"};`

// fakeStart stands in for Glance: a page with one head script, a body script that feed
// text could have smuggled in, and a widget fragment.
func fakeStart(t *testing.T, seen chan<- *http.Request) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.SetCookie(w, &http.Cookie{Name: "session_token", Value: "x", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: homeThemeCookie, Value: "dracula", Path: "/home/"})
		switch r.URL.Path {
		case "/api/pages/home/content/":
			w.Write([]byte(`<head><script>fragment()</script></head><div>feed</div>`))
		default:
			w.Write([]byte(`<!doctype html><html><head><script>` + fakeStartHead + `</script>` +
				`<script type="module" src="/home/static/page.js"></script></head><body><script>evil()</script></body></html>`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

func TestStartPageProxy(t *testing.T) {
	seen := make(chan *http.Request, 10)
	up := fakeStart(t, seen)
	e := newEnv(t, func(c *config.Config) { c.HomeUpstream = up.URL })
	e.setWord(secretWord)
	anon := e.client()
	if r := e.get(anon, "/"); r.status != http.StatusSeeOther || r.header.Get("Location") != "/home/" {
		t.Fatalf("/ with a start page: %d %s", r.status, r.header.Get("Location"))
	}
	r := e.do(anon, "GET", "/home/", nil, map[string]string{"Cookie": "theme=dracula; cps=" + strings.Repeat("A", 43)})
	got := <-seen
	if got.URL.Path != "/" {
		t.Fatalf("upstream path %q, want /", got.URL.Path)
	}
	if ck := got.Header.Get("Cookie"); ck != "theme=dracula" {
		t.Fatalf("upstream got cookies %q, want only the theme", ck)
	}
	csp := r.header.Get("Content-Security-Policy")
	if !strings.Contains(csp, hashOf(fakeStartHead)) || strings.Contains(csp, hashOf("evil()")) || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("page CSP: %s", csp)
	}
	if n := len(r.header.Values("Content-Security-Policy")); n != 1 {
		t.Fatalf("%d CSP headers, want 1", n)
	}
	cookies := strings.Join(r.header.Values("Set-Cookie"), " | ")
	if strings.Contains(cookies, "session_token") || !strings.Contains(cookies, "theme=dracula") || !strings.Contains(cookies, "Path=/home/") {
		t.Fatalf("cookies passed back: %s", cookies)
	}
	frag := e.get(anon, "/home/api/pages/home/content/")
	<-seen
	if strings.Contains(frag.header.Get("Content-Security-Policy"), "sha256-") {
		t.Fatalf("widget fragment allowed a script: %s", frag.header.Get("Content-Security-Policy"))
	}
	if r := e.do(anon, "POST", "/home/", strings.NewReader(""), nil); r.status == http.StatusOK {
		t.Fatalf("POST to the start page: %d, want refused", r.status)
	}
	// Picking a theme is the one write that passes, and only same-origin with a sane key.
	if r := e.do(anon, "POST", "/home/api/set-theme/dracula", strings.NewReader(""), map[string]string{"Sec-Fetch-Site": "same-origin"}); r.status != http.StatusOK {
		t.Fatalf("set theme: %d", r.status)
	}
	if got := <-seen; got.Method != "POST" || got.URL.Path != "/api/set-theme/dracula" {
		t.Fatalf("upstream got %s %s", got.Method, got.URL.Path)
	}
	if r := e.do(anon, "POST", "/home/api/set-theme/dracula", strings.NewReader(""), map[string]string{"Sec-Fetch-Site": "cross-site"}); r.status != http.StatusForbidden {
		t.Fatalf("cross-site set theme: %d, want 403", r.status)
	}
	if r := e.do(anon, "POST", "/home/api/set-theme/Bad%20Key", strings.NewReader(""), map[string]string{"Sec-Fetch-Site": "same-origin"}); r.status != http.StatusNotFound {
		t.Fatalf("bad theme key: %d, want 404", r.status)
	}
}

func TestStartPageIsFencedIn(t *testing.T) {
	seen := make(chan *http.Request, 10)
	up := fakeStart(t, seen)
	e := newEnv(t, func(c *config.Config) { c.HomeUpstream = up.URL })
	r := e.get(e.client(), "/home/")
	<-seen
	csp := r.header.Get("Content-Security-Policy")
	for _, want := range []string{"connect-src http://cp.test/home/;", "script-src http://cp.test/home/static/ ", "frame-src 'none'", "form-action 'none'", "worker-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("start page CSP lacks %q: %s", want, csp)
		}
	}
	if r.header.Get("Cross-Origin-Opener-Policy") != "" {
		t.Error("start page must not carry COOP (cp's pages do, to cut off windows it opens)")
	}
	if r := e.get(e.owner, "/settings"); r.header.Get("Cross-Origin-Opener-Policy") != "same-origin" {
		t.Errorf("cp page COOP: %q", r.header.Get("Cross-Origin-Opener-Policy"))
	}
	// The mux would redirect this to /settings, past the /home/-only fetch rule.
	for _, p := range []string{"/home/..%2fsettings", "/home/..%2Fapi%2Fsettings", "/home/..%5csettings"} {
		if r := e.get(e.owner, p); r.status != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, r.status)
		}
	}
}

func TestStrangersGetPlain404sOnceHidden(t *testing.T) {
	e := newEnv(t)
	anon := e.client()
	if r := e.get(anon, "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatalf("API before hiding: %d, want 401", r.status)
	}
	e.setWord(secretWord)
	missing := e.get(anon, "/no-such-page")
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/shares"}, {"GET", "/api/session"}, {"POST", "/"}, {"POST", "/q"}, {"DELETE", "/login"},
		{"PUT", "/settings"},
	} {
		r := e.do(anon, c.method, c.path, nil, nil)
		if r.status != http.StatusNotFound || r.body != missing.body || r.header.Get("Allow") != "" {
			t.Errorf("%s %s: %d %q (Allow %q), want the plain 404", c.method, c.path, r.status, r.body, r.header.Get("Allow"))
		}
	}
	// A cross-site POST to the hidden login also looks missing, not "refused".
	r := e.do(anon, "POST", "/login", strings.NewReader("username=a"), map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/x-www-form-urlencoded"})
	if r.status != http.StatusNotFound {
		t.Fatalf("cross-site POST /login: %d", r.status)
	}
	// The owner is unaffected.
	if r := e.get(e.owner, "/api/shares"); r.status != http.StatusOK {
		t.Fatalf("owner API: %d", r.status)
	}
}

func TestChangingTheWordEndsOpenTickets(t *testing.T) {
	e := newEnv(t)
	e.setWord(secretWord)
	c := e.client()
	e.search(c, secretWord)
	e.setWord("another secret phrase")
	if r := e.get(c, "/login"); r.status != http.StatusNotFound {
		t.Fatalf("old ticket after a word change: %d", r.status)
	}
}

func TestStartPageDownFallsBackToSearchBox(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	addr := up.URL
	up.Close() // nothing listens there now
	e := newEnv(t, func(c *config.Config) { c.HomeUpstream = addr })
	e.setWord(secretWord)
	r := e.get(e.client(), "/home/")
	if r.status != http.StatusOK || !strings.Contains(r.body, `action="/q"`) {
		t.Fatalf("start page down: %d %.200s", r.status, r.body)
	}
	if r := e.get(e.client(), "/home/static/page.js"); r.status != http.StatusBadGateway {
		t.Fatalf("asset while down: %d, want 502", r.status)
	}
}

func TestStrangerPagesCarryNoBranding(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("hello", map[string]any{"public": true})
	anon := e.client()
	pages := map[string]string{"login": "/login", "share": "/s/" + sh.Slug, "missing share": "/s/" + strings.Repeat("x", 22)}
	bodies := map[string]string{}
	for name, path := range pages {
		bodies[name] = e.get(anon, path).body
	}
	e.setWord(secretWord)
	bodies["start"] = e.get(anon, "/").body
	for name, body := range bodies {
		low := strings.ToLower(body)
		for _, word := range []string{"clipboard", "manifest", "your shares", "· cp<"} {
			if strings.Contains(low, word) {
				t.Errorf("%s page mentions %q", name, word)
			}
		}
	}
}
