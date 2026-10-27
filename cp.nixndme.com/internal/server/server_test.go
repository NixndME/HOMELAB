package server

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cpshare/internal/auth"
	"cpshare/internal/config"
)

func TestDashboardNeedsLogin(t *testing.T) {
	e := newEnv(t)
	anon := e.client()
	r := e.get(anon, "/")
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Fatalf("anonymous dashboard: %d %s", r.status, r.header.Get("Location"))
	}
	if r := e.get(anon, "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous API: %d", r.status)
	}
	if r := e.get(anon, "/settings"); r.status != http.StatusSeeOther {
		t.Fatalf("anonymous settings: %d", r.status)
	}
}

func TestLoginRejectsWrongPasswordAndLocksOut(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	for i := 0; i < 4; i++ {
		if r := e.login(c, testUser, "wrong-password", ""); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	if r := e.login(c, testUser, "wrong-password", ""); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("5th failure should lock: %d", r.StatusCode)
	}
	if r := e.login(c, testUser, testPass, ""); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("locked IP must be refused even with the right password: %d", r.StatusCode)
	}
}

func TestAPIWritesNeedCSRFToken(t *testing.T) {
	e := newEnv(t)
	r := e.do(e.owner, "POST", "/api/uploads", strings.NewReader(`{"name":"a","size":1}`), map[string]string{"Content-Type": "application/json"})
	if r.status != http.StatusForbidden {
		t.Fatalf("missing CSRF accepted: %d", r.status)
	}
}

func TestCrossSiteWritesAreRefused(t *testing.T) {
	e := newEnv(t)
	r := e.do(e.owner, "POST", "/api/uploads", strings.NewReader(`{"name":"a","size":1}`),
		map[string]string{"X-CSRF-Token": e.csrf, "Origin": "https://evil.example", "Content-Type": "application/json"})
	if r.status != http.StatusForbidden {
		t.Fatalf("cross-site write accepted: %d", r.status)
	}
	form := url.Values{"username": {testUser}, "password": {testPass}}
	r = e.do(e.client(), "POST", "/login", strings.NewReader(form.Encode()),
		map[string]string{"Origin": "https://evil.example", "Content-Type": "application/x-www-form-urlencoded"})
	if r.status != http.StatusForbidden {
		t.Fatalf("cross-site login accepted: %d", r.status)
	}
	r = e.do(e.owner, "DELETE", "/api/shares/x", nil, map[string]string{"X-CSRF-Token": e.csrf, "Sec-Fetch-Site": "cross-site"})
	if r.status != http.StatusForbidden {
		t.Fatalf("Sec-Fetch-Site cross-site accepted: %d", r.status)
	}
}

func TestTextShareOwnerAndVisitorView(t *testing.T) {
	e := newEnv(t)
	text := "apiVersion: v1\nkind: ConfigMap\n"
	sh := e.textShare(text, map[string]any{"public": true, "language": "yaml", "title": "my config"})
	if sh.Number <= 0 {
		t.Fatalf("share has no number: %+v", sh)
	}
	if untitled := e.textShare("n", nil); untitled.Number <= sh.Number {
		t.Fatal("share numbers must increase")
	} else if r := e.get(e.owner, "/s/"+untitled.Slug); !strings.Contains(r.body, fmt.Sprintf("Paste #%d", untitled.Number)) {
		t.Fatal(`an untitled text share must be headed "Paste #N"`)
	}
	if len(sh.Slug) != 22 {
		t.Fatalf("slug %q is not 22 chars", sh.Slug)
	}
	r := e.get(e.owner, "/s/"+sh.Slug)
	mustContain(t, r.body, "my config")
	mustContain(t, r.body, "ConfigMap")

	visitor := e.client()
	r = e.get(visitor, "/s/"+sh.Slug)
	if r.status != http.StatusOK {
		t.Fatalf("public share for visitor: %d", r.status)
	}
	mustContain(t, r.body, "ConfigMap")
	if strings.Contains(r.body, `href="/settings"`) || strings.Contains(r.body, "Copy link") {
		t.Fatal("visitor page must not show dashboard navigation or owner actions")
	}
	if raw := e.get(visitor, "/r/"+sh.Slug); raw.body != text {
		t.Fatalf("raw text mismatch: %q", raw.body)
	}
	if r := e.get(visitor, "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatal("visitor reached the API")
	}
}

func TestPrivateShareIsInvisibleToVisitors(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("secret", map[string]any{"public": false})
	visitor := e.client()
	for _, p := range []string{"/s/" + sh.Slug, "/r/" + sh.Slug, "/qr/" + sh.Slug} {
		if r := e.get(visitor, p); r.status != http.StatusNotFound {
			t.Fatalf("%s: visitor got %d", p, r.status)
		}
	}
	if r := e.get(e.owner, "/r/"+sh.Slug); r.body != "secret" {
		t.Fatalf("owner raw: %q", r.body)
	}
}

func TestGlobalPublicSwitchHidesEverything(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("x", map[string]any{"public": true})
	c := defaultSettings()
	c.PublicSharing = false
	if r := e.api("PUT", "/api/settings", c); r.status != http.StatusOK {
		t.Fatalf("save settings: %d %s", r.status, r.body)
	}
	if r := e.get(e.client(), "/s/"+sh.Slug); r.status != http.StatusNotFound {
		t.Fatalf("public sharing off, visitor got %d", r.status)
	}
}

func TestUnknownAndMalformedSlugsLookTheSame(t *testing.T) {
	e := newEnv(t)
	v := e.client()
	a := e.get(v, "/s/AAAAAAAAAAAAAAAAAAAAAA")
	b := e.get(v, "/s/not-a-slug")
	if a.status != http.StatusNotFound || b.status != http.StatusNotFound || a.body != b.body {
		t.Fatal("missing and malformed links must give identical 404s")
	}
}

func TestFileShareDownloadAndZip(t *testing.T) {
	e := newEnv(t)
	data := make([]byte, 3<<20+77) // spans several 1 MiB chunks
	rand.Read(data)
	a := e.upload("big.bin", "application/octet-stream", data)
	b := e.upload("notes.pdf", "application/pdf", []byte("%PDF-1.4 fake"))
	sh := e.create(map[string]any{"kind": "file", "uploads": []string{a, b}, "public": true})
	if sh.Size != int64(len(data)+13) || len(sh.Files) != 2 {
		t.Fatalf("unexpected share %+v", sh)
	}
	v := e.client()
	r := e.get(v, "/d/"+sh.Slug+"/"+itoa64(sh.Files[0].ID))
	if !bytes.Equal([]byte(r.body), data) {
		t.Fatal("downloaded bytes differ")
	}
	if cd := r.header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "big.bin") {
		t.Fatalf("content-disposition %q", cd)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("download content type %q", ct)
	}
	z := e.get(v, "/z/"+sh.Slug)
	if z.status != http.StatusOK || !strings.HasPrefix(z.body, "PK") {
		t.Fatalf("zip: %d", z.status)
	}
}

func TestInlinePreviewOnlyForSafeTypes(t *testing.T) {
	e := newEnv(t)
	h := e.upload("x.html", "text/html", []byte("<script>alert(1)</script>"))
	sh := e.create(map[string]any{"kind": "file", "uploads": []string{h}, "public": true})
	r := e.get(e.client(), "/d/"+sh.Slug+"/"+itoa64(sh.Files[0].ID)+"?inline=1")
	if !strings.HasPrefix(r.header.Get("Content-Disposition"), "attachment") || r.header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("HTML must never render inline: %s %s", r.header.Get("Content-Disposition"), r.header.Get("Content-Type"))
	}
}

func TestLinkShare(t *testing.T) {
	e := newEnv(t)
	sh := e.create(map[string]any{"kind": "link", "link_url": "https://example.com/a?b=c", "public": true})
	r := e.get(e.client(), "/s/"+sh.Slug)
	mustContain(t, r.body, "https://example.com/a?b=c")
	if bad := e.api("POST", "/api/shares", map[string]any{"kind": "link", "link_url": "javascript:alert(1)"}); bad.status != http.StatusBadRequest {
		t.Fatalf("javascript: link accepted: %d", bad.status)
	}
}

func TestLinkShareHoldsManyLinks(t *testing.T) {
	e := newEnv(t)
	sh := e.create(map[string]any{"kind": "link", "public": true,
		"links": []string{"https://a.example/1", " ", "https://b.example/2"}, "link_url": "https://c.example/3\nhttps://d.example/4"})
	if len(sh.Links) != 4 {
		t.Fatalf("want 4 links, got %v", sh.Links)
	}
	r := e.get(e.client(), "/s/"+sh.Slug)
	for _, l := range []string{"a.example/1", "b.example/2", "c.example/3", "d.example/4"} {
		mustContain(t, r.body, l)
	}
	if raw := e.get(e.client(), "/r/"+sh.Slug); strings.Count(raw.body, "\n") != 4 {
		t.Fatalf("raw should list one link per line: %q", raw.body)
	}
	bad := e.api("POST", "/api/shares", map[string]any{"kind": "link", "links": []string{"https://ok.example", "ftp://nope"}})
	if bad.status != http.StatusBadRequest {
		t.Fatalf("a bad link among good ones was accepted: %d", bad.status)
	}
	if r := e.api("POST", "/api/shares", map[string]any{"kind": "link", "links": []string{}}); r.status != http.StatusBadRequest {
		t.Fatalf("empty link share accepted: %d", r.status)
	}
}

func TestSharePasswordUnlock(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("guarded", map[string]any{"public": true, "password": "pw-1234"})
	v := e.client()
	r := e.get(v, "/s/"+sh.Slug)
	mustContain(t, r.body, "Password required")
	if strings.Contains(r.body, "guarded") {
		t.Fatal("content leaked before unlock")
	}
	if raw := e.get(v, "/r/"+sh.Slug); raw.status != http.StatusNotFound {
		t.Fatalf("raw before unlock: %d", raw.status)
	}
	form := url.Values{"password": {"nope"}}.Encode()
	hdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	if r := e.do(v, "POST", "/s/"+sh.Slug+"/unlock", strings.NewReader(form), hdr); r.status != http.StatusUnauthorized {
		t.Fatalf("wrong share password: %d", r.status)
	}
	form = url.Values{"password": {"pw-1234"}}.Encode()
	if r := e.do(v, "POST", "/s/"+sh.Slug+"/unlock", strings.NewReader(form), hdr); r.status != http.StatusSeeOther {
		t.Fatalf("right share password: %d", r.status)
	}
	if raw := e.get(v, "/r/"+sh.Slug); raw.body != "guarded" {
		t.Fatalf("after unlock: %d %q", raw.status, raw.body)
	}
	if raw := e.get(e.client(), "/r/"+sh.Slug); raw.status != http.StatusNotFound {
		t.Fatal("unlock must not carry over to another browser")
	}
}

func TestBurnAfterReadingRevealsOnce(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("one time", map[string]any{"public": true, "burn": true})
	first, second := e.client(), e.client()
	r := e.get(first, "/s/"+sh.Slug)
	mustContain(t, r.body, "Reveal now")
	if strings.Contains(r.body, "one time") {
		t.Fatal("burn share leaked before reveal")
	}
	if r := e.do(first, "POST", "/s/"+sh.Slug+"/reveal", nil, nil); r.status != http.StatusSeeOther {
		t.Fatalf("reveal: %d", r.status)
	}
	if raw := e.get(first, "/r/"+sh.Slug); raw.body != "one time" {
		t.Fatalf("revealer cannot read: %q", raw.body)
	}
	if r := e.get(second, "/s/"+sh.Slug); r.status != http.StatusNotFound {
		t.Fatalf("second visitor got %d", r.status)
	}
	e.advance(burnWindow + time.Minute)
	e.srv.CleanupOnce()
	if e.blobCount() != 0 {
		t.Fatal("burned content still on disk")
	}
}

func TestMaxDownloads(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("twice", map[string]any{"public": true, "max_downloads": 2})
	v := e.client()
	for i := 0; i < 2; i++ {
		if r := e.get(v, "/r/"+sh.Slug); r.body != "twice" {
			t.Fatalf("download %d: %d", i, r.status)
		}
	}
	if r := e.get(v, "/r/"+sh.Slug); r.status != http.StatusNotFound {
		t.Fatalf("third download allowed: %d", r.status)
	}
	e.srv.CleanupOnce()
	if e.blobCount() != 0 {
		t.Fatal("content kept after download limit")
	}
}

func TestExpiryCleanupDeletesContentKeepsHistory(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("short lived", map[string]any{"public": true, "expires_in": 600})
	if e.blobCount() != 1 {
		t.Fatalf("blobs = %d", e.blobCount())
	}
	e.advance(11 * time.Minute)
	if r := e.get(e.client(), "/s/"+sh.Slug); r.status != http.StatusNotFound {
		t.Fatalf("expired share still served before cleanup: %d", r.status)
	}
	e.srv.CleanupOnce()
	if e.blobCount() != 0 {
		t.Fatal("expired blob not deleted")
	}
	var list struct{ Shares []shareJSON }
	e.api("GET", "/api/shares?status=ended", nil).json(t, &list)
	if len(list.Shares) != 1 || list.Shares[0].EndReason != "expired" || list.Shares[0].Active {
		t.Fatalf("history entry missing or wrong: %+v", list.Shares)
	}
	e.advance(31 * 24 * time.Hour) // default history window is 30 days
	e.srv.CleanupOnce()
	e.login(e.owner, testUser, testPass, "") // the 7-day session expired meanwhile
	var after struct{ Shares []shareJSON }
	e.api("GET", "/api/shares?status=ended", nil).json(t, &after)
	if len(after.Shares) != 0 {
		t.Fatal("old history not purged")
	}
}

func TestDeleteRemovesEverything(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("bye", map[string]any{"public": true})
	if r := e.api("DELETE", "/api/shares/"+sh.Slug, nil); r.status != http.StatusNoContent {
		t.Fatalf("delete: %d", r.status)
	}
	if e.blobCount() != 0 {
		t.Fatal("blob left after delete")
	}
	if r := e.get(e.client(), "/s/"+sh.Slug); r.status != http.StatusNotFound {
		t.Fatal("deleted share still served")
	}
}

func TestRegenerateKillsOldLink(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("moving", map[string]any{"public": true})
	var upd shareJSON
	e.api("POST", "/api/shares/"+sh.Slug+"/regenerate", nil).json(t, &upd)
	if upd.Slug == sh.Slug {
		t.Fatal("slug unchanged")
	}
	v := e.client()
	if r := e.get(v, "/s/"+sh.Slug); r.status != http.StatusNotFound {
		t.Fatal("old link still works")
	}
	if r := e.get(v, "/r/"+upd.Slug); r.body != "moving" {
		t.Fatal("new link broken")
	}
}

func TestUpdateShareToggleAndExtend(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("x", map[string]any{"public": false, "expires_in": 600})
	var upd shareJSON
	e.api("PATCH", "/api/shares/"+sh.Slug, map[string]any{"public": true, "expires_in": 86400}).json(t, &upd)
	if !upd.Public || upd.ExpiresAt <= sh.ExpiresAt {
		t.Fatalf("update not applied: %+v", upd)
	}
	if r := e.get(e.client(), "/r/"+sh.Slug); r.body != "x" {
		t.Fatal("share not public after toggle")
	}
}

func TestUploadLimits(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.StorageBytes = 5 << 20 })
	if r := e.api("POST", "/api/uploads", map[string]any{"name": "x", "size": 5 << 20}); r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("over per-file limit: %d", r.status)
	}
	e.upload("a", "", make([]byte, 3<<20))
	if r := e.api("POST", "/api/uploads", map[string]any{"name": "b", "size": 3 << 20}); r.status != http.StatusInsufficientStorage {
		t.Fatalf("over storage cap: %d", r.status)
	}
}

func TestChunksMustArriveInOrderAndNotOverflow(t *testing.T) {
	e := newEnv(t)
	var init struct{ ID string }
	e.api("POST", "/api/uploads", map[string]any{"name": "x", "size": 10}).json(t, &init)
	h := map[string]string{"X-CSRF-Token": e.csrf}
	if r := e.do(e.owner, "PUT", "/api/uploads/"+init.ID+"/1", strings.NewReader("12345"), h); r.status != http.StatusConflict {
		t.Fatalf("out-of-order chunk: %d", r.status)
	}
	if r := e.do(e.owner, "PUT", "/api/uploads/"+init.ID+"/0", strings.NewReader("12345678901"), h); r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("overflowing chunk: %d", r.status)
	}
	if r := e.do(e.owner, "PUT", "/api/uploads/"+init.ID+"/0", strings.NewReader("12345"), h); r.status != http.StatusOK {
		t.Fatalf("first chunk: %d", r.status)
	}
	if r := e.do(e.owner, "PUT", "/api/uploads/"+init.ID+"/0", strings.NewReader("12345"), h); r.status != http.StatusOK {
		t.Fatalf("retried chunk must be acknowledged: %d", r.status)
	}
	if r := e.api("POST", "/api/shares", map[string]any{"kind": "file", "uploads": []string{init.ID}}); r.status != http.StatusBadRequest {
		t.Fatalf("incomplete upload accepted: %d", r.status)
	}
	e.do(e.owner, "PUT", "/api/uploads/"+init.ID+"/1", strings.NewReader("67890"), h)
	sh := e.create(map[string]any{"kind": "file", "uploads": []string{init.ID}})
	if r := e.get(e.owner, "/d/"+sh.Slug+"/"+itoa64(sh.Files[0].ID)); r.body != "1234567890" {
		t.Fatalf("reassembled %q", r.body)
	}
}

func TestStaleUploadsAndOrphansAreSwept(t *testing.T) {
	e := newEnv(t)
	e.upload("abandoned", "", []byte("data"))
	e.advance(staleUploadAge + time.Hour)
	e.srv.CleanupOnce()
	if e.blobCount() != 0 {
		t.Fatal("abandoned upload not removed")
	}
}

func TestChangePasswordEndsOtherSessions(t *testing.T) {
	e := newEnv(t)
	other := e.client()
	e.login(other, testUser, testPass, "")
	if r := e.api("POST", "/api/account/password", map[string]string{"current": "wrong", "new": "brand-new-password"}); r.status != http.StatusForbidden {
		t.Fatalf("wrong current password: %d", r.status)
	}
	if r := e.api("POST", "/api/account/password", map[string]string{"current": testPass, "new": "short"}); r.status != http.StatusBadRequest {
		t.Fatalf("short password: %d", r.status)
	}
	if r := e.api("POST", "/api/account/password", map[string]string{"current": testPass, "new": "brand-new-password"}); r.status != http.StatusOK {
		t.Fatalf("change: %d %s", r.status, r.body)
	}
	if r := e.get(other, "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatal("other session survived a password change")
	}
	if r := e.get(e.owner, "/api/shares"); r.status != http.StatusOK {
		t.Fatal("current session must survive")
	}
	if r := e.login(e.client(), testUser, "brand-new-password", ""); r.StatusCode != http.StatusSeeOther {
		t.Fatal("new password does not work")
	}
}

func TestTOTPFlow(t *testing.T) {
	e := newEnv(t)
	var setup struct{ Secret string }
	e.api("POST", "/api/account/totp/setup", nil).json(t, &setup)
	if r := e.api("POST", "/api/account/totp/enable", map[string]string{"code": "000000"}); r.status != http.StatusBadRequest && auth.TOTPCode(setup.Secret, time.Now()) != "000000" {
		t.Fatalf("bad code accepted: %d", r.status)
	}
	if r := e.api("POST", "/api/account/totp/enable", map[string]string{"code": auth.TOTPCode(setup.Secret, time.Now())}); r.status != http.StatusOK {
		t.Fatalf("enable: %d %s", r.status, r.body)
	}
	if r := e.login(e.client(), testUser, testPass, ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatal("login without 2FA code accepted")
	}
	// The code used to enable 2FA cannot be replayed; the next step's code works.
	next := auth.TOTPCode(setup.Secret, time.Now().Add(30*time.Second))
	if r := e.login(e.client(), testUser, testPass, next); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login with code: %d", r.StatusCode)
	}
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t)
	c := defaultSettings()
	c.DefaultExpiry = 5
	if r := e.api("PUT", "/api/settings", c); r.status != http.StatusBadRequest {
		t.Fatalf("invalid settings accepted: %d", r.status)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	r := e.get(e.client(), "/login")
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
		if r.header.Get(h) == "" {
			t.Fatalf("missing header %s", h)
		}
	}
	if r.header.Get("Referrer-Policy") != "same-origin" {
		t.Fatal("share URLs could leak through Referer")
	}
}

func TestReadHeadTruncatesLargeText(t *testing.T) {
	big := strings.Repeat("line of text\n", previewLines+10)
	text, lines, truncated, err := readHead(strings.NewReader(big), previewBytes, previewLines)
	if err != nil || !truncated || lines != previewLines || !strings.HasSuffix(text, "\n") {
		t.Fatalf("lines=%d truncated=%v err=%v", lines, truncated, err)
	}
	text, _, truncated, _ = readHead(strings.NewReader("a\nb"), previewBytes, previewLines)
	if truncated || text != "a\nb" {
		t.Fatalf("small text: %q %v", text, truncated)
	}
	long := strings.Repeat("é", previewBytes) // one huge line, 2 bytes per rune
	text, _, truncated, _ = readHead(strings.NewReader(long), previewBytes, previewLines)
	if !truncated || len(text) > previewBytes || !strings.HasSuffix(text, "é") {
		t.Fatal("long line cut badly")
	}
}

func itoa64(n int64) string { return strconvFormat(n) }

func TestEmbedViewHasNoHeader(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("inline me", map[string]any{"title": "embedded"})
	full := e.get(e.owner, "/s/"+sh.Slug)
	embed := e.get(e.owner, "/s/"+sh.Slug+"?embed=1")
	mustContain(t, full.body, `class="topbar"`)
	mustContain(t, embed.body, "inline me")
	if strings.Contains(embed.body, `class="topbar"`) || strings.Contains(embed.body, "Copy link") {
		t.Fatal("embedded view must not repeat the header or owner buttons")
	}
}

func TestNewSharesArePrivateByDefault(t *testing.T) {
	e := newEnv(t)
	sh := e.create(map[string]any{"kind": "text", "uploads": []string{e.upload("paste.txt", "text/plain", []byte("x"))}})
	if sh.Public {
		t.Fatal("a share created without choosing must be private")
	}
	if r := e.get(e.client(), "/s/"+sh.Slug); r.status != http.StatusNotFound {
		t.Fatalf("visitor reached a default share: %d", r.status)
	}
}

func TestAssetURLsAreVersioned(t *testing.T) {
	e := newEnv(t)
	r := e.get(e.owner, "/")
	mustContain(t, r.body, "/static/dashboard.js?v="+assetVersion)
	mustContain(t, r.body, "/static/app.css?v="+assetVersion)
	if strings.Contains(r.body, `"/static/app.js"`) {
		t.Fatal("unversioned asset URL would be cached stale by Cloudflare")
	}
}

func TestMixedShareHoldsTextFilesAndLinks(t *testing.T) {
	e := newEnv(t)
	text := e.upload("ignored-name.txt", "text/plain", []byte("kubectl get pods\n"))
	f1 := e.upload("a.pdf", "application/pdf", []byte("%PDF-1.4 a"))
	f2 := e.upload("b.bin", "", []byte("bbbb"))
	sh := e.create(map[string]any{"text_upload": text, "uploads": []string{f1, f2},
		"links": []string{"https://one.example", "https://two.example"}, "public": true})
	if sh.Text == nil || len(sh.Files) != 2 || len(sh.Links) != 2 || sh.Kind != "text" {
		t.Fatalf("mixed share not stored as expected: %+v", sh)
	}
	v := e.client()
	page := e.get(v, "/s/"+sh.Slug)
	for _, want := range []string{"kubectl get pods", "a.pdf", "b.bin", "one.example", "two.example", fmt.Sprintf("Share #%d", sh.Number), "text · 2 files · 2 links"} {
		mustContain(t, page.body, want)
	}
	if raw := e.get(v, "/r/"+sh.Slug); raw.body != "kubectl get pods\n" {
		t.Fatalf("raw of a mixed share should be its text: %q", raw.body)
	}
	if z := e.get(v, "/z/"+sh.Slug); !strings.HasPrefix(z.body, "PK") || !strings.Contains(z.body, "paste.txt") || !strings.Contains(z.body, "a.pdf") {
		t.Fatal("zip should hold the text and every file")
	}
}

func TestMixedShareValidation(t *testing.T) {
	e := newEnv(t)
	if r := e.api("POST", "/api/shares", map[string]any{"links": []string{}}); r.status != http.StatusBadRequest {
		t.Fatalf("empty share accepted: %d", r.status)
	}
	f := e.upload("a", "", []byte("x"))
	if r := e.api("POST", "/api/shares", map[string]any{"uploads": []string{f}, "links": []string{"ftp://bad"}}); r.status != http.StatusBadRequest {
		t.Fatalf("bad link in a mixed share accepted: %d", r.status)
	}
	if r := e.api("POST", "/api/shares", map[string]any{"text_upload": f, "uploads": []string{f}}); r.status != http.StatusBadRequest {
		t.Fatalf("the same upload used as text and file accepted: %d", r.status)
	}
	sh := e.create(map[string]any{"links": []string{"https://only.example"}})
	if sh.Kind != "link" || sh.Text != nil || len(sh.Files) != 0 {
		t.Fatalf("links-only share: %+v", sh)
	}
}

func TestZipButtonAtTopAndPreviewTypes(t *testing.T) {
	e := newEnv(t)
	a := e.upload("a.txt", "text/plain", []byte("aaa"))
	b := e.upload("b.pdf", "application/pdf", []byte("%PDF"))
	c := e.upload("c.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", []byte("PK"))
	sh := e.create(map[string]any{"uploads": []string{a, b, c}, "public": true})
	page := e.get(e.client(), "/s/"+sh.Slug).body
	head := page[strings.Index(page, `class="share-head"`):strings.Index(page, `class="file-list"`)]
	mustContain(t, head, "/z/"+sh.Slug)
	mustContain(t, page, `data-type="text" data-name="a.txt"`)
	mustContain(t, page, `data-type="pdf" data-name="b.pdf"`)
	if strings.Contains(page, `data-name="c.docx"`) {
		t.Fatal("a .docx must only download, never preview")
	}
	embed := e.get(e.owner, "/s/"+sh.Slug+"?embed=1").body
	mustContain(t, embed, `class="share-bar"`)
}

func TestRenameAndBackToAutomaticName(t *testing.T) {
	e := newEnv(t)
	sh := e.textShare("x", nil)
	var upd shareJSON
	e.api("PATCH", "/api/shares/"+sh.Slug, map[string]any{"title": "  HKS behind proxy  "}).json(t, &upd)
	if upd.Title != "HKS behind proxy" {
		t.Fatalf("rename: %q", upd.Title)
	}
	mustContain(t, e.get(e.owner, "/s/"+sh.Slug).body, "HKS behind proxy")
	e.api("PATCH", "/api/shares/"+sh.Slug, map[string]any{"title": ""}).json(t, &upd)
	if upd.Title != "" {
		t.Fatalf("clearing the name: %q", upd.Title)
	}
	mustContain(t, e.get(e.owner, "/s/"+sh.Slug).body, fmt.Sprintf("Paste #%d", sh.Number))
}
