package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cpshare/internal/auth"
)

// newToken creates an API token through the owner API and returns its secret.
func (e *env) newToken(name string) string {
	e.t.Helper()
	r := e.api("POST", "/api/account/tokens", map[string]any{"name": name, "password": testPass})
	if r.status != http.StatusCreated {
		e.t.Fatalf("create token: %d %s", r.status, r.body)
	}
	var out struct {
		Token string `json:"token"`
	}
	r.json(e.t, &out)
	return out.Token
}

// bearer calls the API with only a token: no cookies, no CSRF header (like curl).
func (e *env) bearer(token, method, path string, body []byte, contentType string) result {
	e.t.Helper()
	h := map[string]string{"Authorization": "Bearer " + token}
	if contentType != "" {
		h["Content-Type"] = contentType
	}
	return e.do(e.client(), method, path, bytes.NewReader(body), h) // fresh: no cookies, no redirects
}

func (e *env) bearerJSON(token, method, path string, v any) result {
	e.t.Helper()
	b, _ := json.Marshal(v)
	return e.bearer(token, method, path, b, "application/json")
}

// tokenUpload uploads data in chunks with a token, as cpup does.
func (e *env) tokenUpload(token, name string, data []byte) string {
	e.t.Helper()
	r := e.bearerJSON(token, "POST", "/api/uploads", map[string]any{"name": name, "size": len(data), "mime": "application/octet-stream"})
	if r.status != http.StatusCreated {
		e.t.Fatalf("token start upload: %d %s", r.status, r.body)
	}
	var init struct {
		ID    string `json:"id"`
		Chunk int    `json:"chunk_bytes"`
	}
	r.json(e.t, &init)
	for i, off := 0, 0; off < len(data); i, off = i+1, off+init.Chunk {
		end := min(off+init.Chunk, len(data))
		res := e.bearer(token, "PUT", fmt.Sprintf("/api/uploads/%s/%d", init.ID, i), data[off:end], "application/octet-stream")
		if res.status != http.StatusOK {
			e.t.Fatalf("token chunk %d: %d %s", i, res.status, res.body)
		}
	}
	return init.ID
}

func TestTokenCreatesShareWithoutBrowserLogin(t *testing.T) {
	e := newEnv(t)
	token := e.newToken("laptop")
	if !strings.HasPrefix(token, tokenPrefix) || len(token) != len(tokenPrefix)+43 {
		t.Fatalf("unexpected token format %q", token)
	}
	data := bytes.Repeat([]byte("0123456789abcdef"), (3<<20)/16) // 3 MB = 3 chunks of 1 MB
	id := e.tokenUpload(token, "big.bin", data)
	text := e.tokenUpload(token, "paste.txt", []byte("hello from cpup\n"))
	r := e.bearerJSON(token, "POST", "/api/shares", map[string]any{
		"title": "from script", "text_upload": text, "uploads": []string{id},
		"links": []string{"https://example.org/x"}, "expires_in": 3600, "public": true,
	})
	if r.status != http.StatusCreated {
		t.Fatalf("token create share: %d %s", r.status, r.body)
	}
	var sh shareJSON
	r.json(t, &sh)
	if sh.Title != "from script" || len(sh.Files) != 1 || sh.Files[0].Size != int64(len(data)) || sh.Text == nil || len(sh.Links) != 1 || !sh.Public {
		t.Fatalf("share not as requested: %+v", sh)
	}
	anon := e.client()
	dl := e.get(anon, fmt.Sprintf("/d/%s/%d", sh.Slug, sh.Files[0].ID))
	if dl.status != http.StatusOK || dl.body != string(data) {
		t.Fatalf("download: %d, %d bytes", dl.status, len(dl.body))
	}
	if raw := e.get(anon, "/r/"+sh.Slug); raw.body != "hello from cpup\n" {
		t.Fatalf("raw text: %q", raw.body)
	}
}

func TestTokenCannotDoAnythingButCreate(t *testing.T) {
	e := newEnv(t)
	token := e.newToken("ci")
	sh := e.textShare("secret text", nil)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/shares"},
		{"GET", "/api/settings"},
		{"PUT", "/api/settings"},
		{"DELETE", "/api/shares/" + sh.Slug},
		{"PATCH", "/api/shares/" + sh.Slug},
		{"GET", "/api/shares/" + sh.Slug + "/log"},
		{"GET", "/api/account/tokens"},
		{"POST", "/api/account/tokens"},
		{"GET", "/api/account/sessions"},
		{"POST", "/api/account/password"},
		{"GET", "/"},
	} {
		r := e.bearerJSON(token, c.method, c.path, map[string]any{})
		if r.status == http.StatusOK || r.status == http.StatusCreated || r.status == http.StatusNoContent {
			t.Errorf("%s %s with a token: %d, want refused", c.method, c.path, r.status)
		}
	}
	// A private share stays private for token holders.
	if r := e.bearer(token, "GET", "/r/"+sh.Slug, nil, ""); r.status != http.StatusNotFound {
		t.Errorf("private share via token: %d, want 404", r.status)
	}
}

func TestWrongRevokedOrMalformedTokenRefused(t *testing.T) {
	e := newEnv(t)
	token := e.newToken("old")
	body := map[string]any{"name": "a", "size": 1, "mime": "text/plain"}
	for _, bad := range []string{
		tokenPrefix + strings.Repeat("A", 43), // well formed, unknown
		"cpt_short",
		token + "x",
		strings.TrimPrefix(token, tokenPrefix), // missing prefix
	} {
		if r := e.bearerJSON(bad, "POST", "/api/uploads", body); r.status != http.StatusUnauthorized {
			t.Errorf("token %q: %d, want 401", bad, r.status)
		}
	}
	var list struct {
		Tokens []tokenJSON `json:"tokens"`
	}
	e.api("GET", "/api/account/tokens", nil).json(t, &list)
	if len(list.Tokens) != 1 || strings.Contains(fmt.Sprint(list), token) {
		t.Fatalf("list: %+v", list)
	}
	if r := e.api("DELETE", fmt.Sprintf("/api/account/tokens/%d", list.Tokens[0].ID), nil); r.status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	if r := e.bearerJSON(token, "POST", "/api/uploads", body); r.status != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d, want 401", r.status)
	}
}

func TestBadTokensLockOutTheIP(t *testing.T) {
	e := newEnv(t)
	good := e.newToken("good")
	bad := tokenPrefix + strings.Repeat("B", 43)
	body := map[string]any{"name": "a", "size": 1, "mime": "text/plain"}
	for i := 0; i < 10; i++ {
		e.bearerJSON(bad, "POST", "/api/uploads", body)
	}
	if r := e.bearerJSON(good, "POST", "/api/uploads", body); r.status != http.StatusTooManyRequests {
		t.Fatalf("after 10 bad tokens: %d, want 429", r.status)
	}
}

func TestTokenCreationNeedsPasswordAndHasALimit(t *testing.T) {
	e := newEnv(t)
	if r := e.api("POST", "/api/account/tokens", map[string]any{"name": "x", "password": "wrong"}); r.status != http.StatusForbidden {
		t.Fatalf("wrong password: %d", r.status)
	}
	if r := e.api("POST", "/api/account/tokens", map[string]any{"name": "  ", "password": testPass}); r.status != http.StatusBadRequest {
		t.Fatalf("empty name: %d", r.status)
	}
	// Without the CSRF header a logged-in browser cannot make one either.
	b, _ := json.Marshal(map[string]any{"name": "x", "password": testPass})
	if r := e.do(e.owner, "POST", "/api/account/tokens", bytes.NewReader(b), map[string]string{"Content-Type": "application/json"}); r.status != http.StatusForbidden {
		t.Fatalf("no CSRF: %d", r.status)
	}
	for i := 0; i < maxAPITokens; i++ {
		e.newToken(fmt.Sprint("t", i))
	}
	if r := e.api("POST", "/api/account/tokens", map[string]any{"name": "one too many", "password": testPass}); r.status != http.StatusConflict {
		t.Fatalf("over the limit: %d %s", r.status, r.body)
	}
}

func TestTokenUseIsRecorded(t *testing.T) {
	e := newEnv(t)
	token := e.newToken("phone")
	e.tokenUpload(token, "a.txt", []byte("a"))
	var list struct {
		Tokens []tokenJSON `json:"tokens"`
	}
	e.api("GET", "/api/account/tokens", nil).json(t, &list)
	if list.Tokens[0].LastUsed == 0 || list.Tokens[0].LastIP == "" {
		t.Fatalf("last use not recorded: %+v", list.Tokens[0])
	}
}

func TestResetCredentialsRevokesTokens(t *testing.T) {
	e := newEnv(t)
	token := e.newToken("laptop")
	hash, _ := auth.HashPassword("brand-new-password-1")
	if err := e.st.ResetCredentials(hash); err != nil {
		t.Fatal(err)
	}
	if r := e.bearerJSON(token, "POST", "/api/uploads", map[string]any{"name": "a", "size": 1}); r.status != http.StatusUnauthorized {
		t.Fatalf("token after reset-password: %d, want 401", r.status)
	}
}

func TestNonBearerAuthorizationUsesTheBrowserLogin(t *testing.T) {
	e := newEnv(t)
	b, _ := json.Marshal(map[string]any{"name": "a.txt", "size": 1, "mime": "text/plain"})
	// e.g. a proxy adding its own Basic header: the session + CSRF path still decides.
	r := e.do(e.owner, "POST", "/api/uploads", bytes.NewReader(b), map[string]string{
		"Content-Type": "application/json", "X-CSRF-Token": e.csrf, "Authorization": "Basic dXNlcjpwYXNz"})
	if r.status != http.StatusCreated {
		t.Fatalf("session upload with a Basic header: %d %s", r.status, r.body)
	}
	for i := 0; i < 12; i++ { // never counted as bad tokens
		e.do(e.client(), "POST", "/api/uploads", bytes.NewReader(b), map[string]string{"Authorization": "Basic x"})
	}
	if r := e.bearerJSON(e.newToken("ok"), "POST", "/api/uploads", map[string]any{"name": "a", "size": 1}); r.status != http.StatusCreated {
		t.Fatalf("token after Basic headers: %d %s", r.status, r.body)
	}
}

func TestShareTargetFallbackAcceptsCrossSiteHeader(t *testing.T) {
	e := newEnv(t)
	r := e.do(e.client(), "POST", "/share-target", strings.NewReader(""), map[string]string{"Sec-Fetch-Site": "cross-site"})
	if r.status != http.StatusSeeOther {
		t.Fatalf("share-target with Sec-Fetch-Site cross-site: %d", r.status)
	}
	// Every other write still refuses cross-site requests.
	if r := e.do(e.owner, "POST", "/api/uploads", strings.NewReader("{}"), map[string]string{"Sec-Fetch-Site": "cross-site", "X-CSRF-Token": e.csrf}); r.status != http.StatusForbidden {
		t.Fatalf("cross-site API write: %d, want 403", r.status)
	}
}

func TestBrowserUploadsStillNeedCSRF(t *testing.T) {
	e := newEnv(t)
	b, _ := json.Marshal(map[string]any{"name": "a", "size": 1})
	r := e.do(e.owner, "POST", "/api/uploads", bytes.NewReader(b), map[string]string{"Content-Type": "application/json"})
	if r.status != http.StatusForbidden {
		t.Fatalf("session upload without CSRF: %d, want 403", r.status)
	}
}

func TestAppFilesAreServed(t *testing.T) {
	e := newEnv(t)
	anon := e.client()
	m := e.get(anon, "/manifest.webmanifest")
	if m.status != http.StatusOK || !strings.HasPrefix(m.header.Get("Content-Type"), "application/manifest+json") {
		t.Fatalf("manifest: %d %s", m.status, m.header.Get("Content-Type"))
	}
	var man struct {
		ShareTarget struct {
			Action string `json:"action"`
			Method string `json:"method"`
		} `json:"share_target"`
		Icons []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	m.json(t, &man)
	if man.ShareTarget.Action != "/share-target" || man.ShareTarget.Method != "POST" {
		t.Fatalf("share_target: %+v", man.ShareTarget)
	}
	for _, ic := range man.Icons {
		if r := e.get(anon, ic.Src); r.status != http.StatusOK || r.header.Get("Content-Type") != "image/png" {
			t.Errorf("icon %s: %d %s", ic.Src, r.status, r.header.Get("Content-Type"))
		}
	}
	sw := e.get(anon, "/sw.js")
	if sw.status != http.StatusOK || !strings.HasPrefix(sw.header.Get("Content-Type"), "text/javascript") || sw.header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("sw.js: %d %v", sw.status, sw.header)
	}
	mustContain(t, sw.body, "/share-target")
	if sw.header.Get("Content-Security-Policy") == "" {
		t.Fatal("sw.js lost the CSP header")
	}
	if up := e.get(anon, "/cpup"); up.status != http.StatusNotFound {
		t.Fatalf("cpup for a stranger: %d, want 404", up.status)
	}
	for name, r := range map[string]result{
		"token": e.bearer(e.newToken("dl"), "GET", "/cpup", nil, ""),
		"owner": e.get(e.owner, "/cpup"),
	} {
		if r.status != http.StatusOK || !strings.HasPrefix(r.body, "#!/usr/bin/env bash") {
			t.Fatalf("cpup for %s: %d %.40q", name, r.status, r.body)
		}
	}
	// Without the service worker, a share-sheet POST just lands on the dashboard.
	fb := e.do(anon, "POST", "/share-target", strings.NewReader(""), map[string]string{"Sec-Fetch-Site": "none"})
	if fb.status != http.StatusSeeOther || fb.header.Get("Location") != "/?shared=lost" {
		t.Fatalf("share-target fallback: %d %s", fb.status, fb.header.Get("Location"))
	}
	page := e.get(e.owner, "/")
	mustContain(t, page.body, `<link rel="manifest" href="/manifest.webmanifest">`)
}
