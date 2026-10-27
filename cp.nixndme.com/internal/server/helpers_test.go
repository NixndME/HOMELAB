package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"cpshare/internal/auth"
	"cpshare/internal/config"
	"cpshare/internal/store"
	"cpshare/internal/vault"
)

const (
	testUser = "owner"
	testPass = "owner-password-123"
)

type env struct {
	t     *testing.T
	srv   *Server
	st    *store.Store
	http  *httptest.Server
	owner *http.Client
	csrf  string
	clock time.Time
	dir   string
}

func newEnv(t *testing.T, tweak ...func(*config.Config)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{Addr: ":0", DataDir: dir, KeyFile: filepath.Join(dir, "secret.key"), BaseURL: "http://cp.test",
		MaxFileBytes: 4 << 20, ChunkBytes: 1 << 20, StorageBytes: 64 << 20, MaxFiles: 5}
	for _, f := range tweak {
		f(&cfg)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, err := vault.Open(filepath.Join(dir, "blobs"), cfg.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword(testPass)
	if err := st.CreateUser(testUser, hash); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, st, v, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, srv: srv, st: st, clock: time.Now(), dir: dir}
	st.SetClock(func() time.Time { return e.clock })
	e.http = httptest.NewServer(srv.Handler())
	t.Cleanup(e.http.Close)
	e.owner = e.client()
	e.login(e.owner, testUser, testPass, "")
	return e
}

func (e *env) advance(d time.Duration) { e.clock = e.clock.Add(d) }

func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var csrfRe = regexp.MustCompile(`<meta name="csrf" content="([A-Za-z0-9]+)">`)

func (e *env) login(c *http.Client, user, pass, code string) *http.Response {
	e.t.Helper()
	resp, err := c.PostForm(e.http.URL+"/login", url.Values{"username": {user}, "password": {pass}, "code": {code}})
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther && c == e.owner {
		page := e.get(c, "/")
		m := csrfRe.FindStringSubmatch(page.body)
		if m == nil {
			e.t.Fatal("no CSRF token on dashboard")
		}
		e.csrf = m[1]
	}
	return resp
}

type result struct {
	status int
	body   string
	header http.Header
}

func (r result) json(t *testing.T, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), dst); err != nil {
		t.Fatalf("bad JSON %q: %v", r.body, err)
	}
}

func (e *env) do(c *http.Client, method, path string, body io.Reader, headers map[string]string) result {
	e.t.Helper()
	req, err := http.NewRequest(method, e.http.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, body: string(b), header: resp.Header}
}

func (e *env) get(c *http.Client, path string) result { return e.do(c, "GET", path, nil, nil) }

// api calls the owner JSON API with the CSRF header.
func (e *env) api(method, path string, body any) result {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	return e.do(e.owner, method, path, rd, map[string]string{"X-CSRF-Token": e.csrf, "Content-Type": "application/json"})
}

// upload sends data through the chunked upload API and returns the upload id.
func (e *env) upload(name, mime string, data []byte) string {
	e.t.Helper()
	r := e.api("POST", "/api/uploads", map[string]any{"name": name, "size": len(data), "mime": mime})
	if r.status != http.StatusCreated {
		e.t.Fatalf("start upload: %d %s", r.status, r.body)
	}
	var init struct {
		ID    string `json:"id"`
		Chunk int    `json:"chunk_bytes"`
	}
	r.json(e.t, &init)
	for i, off := 0, 0; off < len(data); i, off = i+1, off+init.Chunk {
		end := min(off+init.Chunk, len(data))
		res := e.do(e.owner, "PUT", fmt.Sprintf("/api/uploads/%s/%d", init.ID, i), bytes.NewReader(data[off:end]),
			map[string]string{"X-CSRF-Token": e.csrf})
		if res.status != http.StatusOK {
			e.t.Fatalf("chunk %d: %d %s", i, res.status, res.body)
		}
	}
	return init.ID
}

func (e *env) create(req map[string]any) shareJSON {
	e.t.Helper()
	r := e.api("POST", "/api/shares", req)
	if r.status != http.StatusCreated {
		e.t.Fatalf("create share: %d %s", r.status, r.body)
	}
	var sh shareJSON
	r.json(e.t, &sh)
	return sh
}

func (e *env) textShare(text string, extra map[string]any) shareJSON {
	e.t.Helper()
	req := map[string]any{"kind": "text", "uploads": []string{e.upload("paste.txt", "text/plain", []byte(text))}}
	for k, v := range extra {
		req[k] = v
	}
	return e.create(req)
}

func (e *env) blobCount() int {
	n := 0
	filepath.WalkDir(filepath.Join(e.dir, "blobs"), func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("response does not contain %q:\n%.400s", want, body)
	}
}

func strconvFormat(n int64) string { return fmt.Sprint(n) }
