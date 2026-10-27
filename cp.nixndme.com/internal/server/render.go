package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"

	"cpshare/internal/store"
	"cpshare/web"
)

// The share page shows at most this much text; "Copy all" and "Download" always give the full text,
// so a million-line paste never freezes the browser.
const (
	previewBytes   = 512 << 10
	previewLines   = 5000
	highlightBytes = 256 << 10
)

var pageNames = []string{"login", "dashboard", "settings", "share", "gone"}

// assetVersion is a hash of every embedded static file. Asset URLs carry it (?v=…), so a new
// build always gets fresh URLs and neither Cloudflare's edge nor the browser serves stale JS/CSS.
var assetVersion = func() string {
	h := sha256.New()
	_ = fs.WalkDir(web.FS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(web.FS, p)
		if err != nil {
			return err
		}
		h.Write([]byte(p))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

// previewType says how the file preview window shows a file: "image", "pdf", "text", or ""
// for types that are only ever downloaded.
func previewType(m string) string {
	base, _, _ := mime.ParseMediaType(m)
	switch {
	case !inlineSafe[base]:
		return ""
	case base == "application/pdf":
		return "pdf"
	case strings.HasPrefix(base, "image/"):
		return "image"
	default:
		return "text"
	}
}

// heading is a share's title, or a name people can say out loud: "Paste #12", "Share #14".
func heading(sh store.Share) string {
	if sh.Title != "" {
		return sh.Title
	}
	_, hasText := sh.Text()
	files, links := len(sh.Attachments()), len(sh.Links())
	n := strconv.FormatInt(sh.PK, 10)
	switch {
	case (hasText && (files > 0 || links > 0)) || (files > 0 && links > 0):
		return "Share #" + n
	case hasText:
		return "Paste #" + n
	case files > 0:
		return "Files #" + n
	default:
		return "Links #" + n
	}
}

// parts summarises what a share holds, e.g. "text · 2 files · 3 links".
func parts(sh store.Share) string {
	var out []string
	if _, ok := sh.Text(); ok {
		out = append(out, "text")
	}
	if n := len(sh.Attachments()); n > 0 {
		out = append(out, plural(n, "file"))
	}
	if n := len(sh.Links()); n > 0 {
		out = append(out, plural(n, "link"))
	}
	return strings.Join(out, " · ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func loadTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"bytes":    humanBytes,
		"datetime": func(t int64) string { return time.Unix(t, 0).UTC().Format("2006-01-02 15:04 UTC") },
		"iso":      func(t int64) string { return time.Unix(t, 0).UTC().Format(time.RFC3339) },
		"inline": func(m string) bool {
			base, _, _ := mime.ParseMediaType(m)
			return inlineSafe[base]
		},
		"previewType": previewType,
		"asset":       func(name string) string { return "/static/" + name + "?v=" + assetVersion },
		"heading":     heading,
		"parts":       parts,
	}
	pages := map[string]*template.Template{}
	for _, name := range pageNames {
		t, err := template.New(name).Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		pages[name] = t
	}
	return pages, nil
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data map[string]any) {
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "layout", data); err != nil {
		s.log.Error("render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func staticHandler() http.Handler {
	sub, err := fs.Sub(web.FS, "static")
	if err != nil {
		panic(err) // embedded at build time; cannot fail at runtime
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	})
}

func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "dashboard", map[string]any{"CSRF": ownerFrom(r.Context()).CSRF, "Nav": "dashboard"})
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "settings", map[string]any{"CSRF": ownerFrom(r.Context()).CSRF, "Nav": "settings"})
}

// Preview is the part of a text share shown on its page.
type Preview struct {
	HTML      template.HTML // highlighted code, when highlighting applies
	Text      string        // plain text otherwise
	Lines     int
	Truncated bool
}

func (s *Server) preview(f store.File, language string) (Preview, error) {
	rd, err := s.vault.Reader(f.BlobID, f.Size)
	if err != nil {
		return Preview{}, err
	}
	defer rd.Close()
	text, lines, truncated, err := readHead(rd, previewBytes, previewLines)
	if err != nil {
		return Preview{}, err
	}
	p := Preview{Text: text, Lines: lines, Truncated: truncated || f.Size > int64(len(text))}
	if len(text) <= highlightBytes {
		if h, ok := highlight(text, language, f.Name); ok {
			p.HTML, p.Text = h, ""
		}
	}
	return p, nil
}

// readHead reads at most maxBytes and maxLines, cutting at a line boundary and never in the
// middle of a UTF-8 character.
func readHead(r io.Reader, maxBytes, maxLines int) (string, int, bool, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var b strings.Builder
	lines := 0
	for lines < maxLines {
		line, err := br.ReadString('\n')
		if b.Len()+len(line) > maxBytes {
			rest := line[:maxBytes-b.Len()]
			for len(rest) > 0 && !utf8.ValidString(rest) {
				rest = rest[:len(rest)-1]
			}
			b.WriteString(rest)
			return b.String(), lines + 1, true, nil
		}
		b.WriteString(line)
		if line != "" {
			lines++
		}
		if err == io.EOF {
			return b.String(), lines, false, nil
		}
		if err != nil {
			return "", 0, false, err
		}
	}
	_, err := br.Peek(1)
	return b.String(), lines, err == nil, nil
}

var (
	chromaFormatter = chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithLineNumbers(true),
		chromahtml.LineNumbersInTable(true))
	chromaOnce sync.Once
	chromaCSSB []byte
)

func highlight(text, language, filename string) (template.HTML, bool) {
	var lexer chroma.Lexer
	if language != "" {
		lexer = lexers.Get(language)
	}
	if lexer == nil && filename != "" && filename != "paste.txt" {
		lexer = lexers.Match(filename)
	}
	if lexer == nil {
		return "", false
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, text)
	if err != nil {
		return "", false
	}
	var buf bytes.Buffer
	if err := chromaFormatter.Format(&buf, styles.Get("github"), it); err != nil {
		return "", false
	}
	return template.HTML(buf.String()), true // chroma escapes every token
}

var chromaSelector = regexp.MustCompile(`(^|\n)(/\*[^*]*\*/ )?\.`)

// chromaCSS serves the light theme plus a dark theme scoped to [data-theme="dark"].
func (s *Server) chromaCSS(w http.ResponseWriter, _ *http.Request) {
	chromaOnce.Do(func() {
		var light, dark bytes.Buffer
		_ = chromaFormatter.WriteCSS(&light, styles.Get("github"))
		_ = chromaFormatter.WriteCSS(&dark, styles.Get("github-dark"))
		scoped := chromaSelector.ReplaceAllString(dark.String(), `$1$2[data-theme="dark"] .`)
		chromaCSSB = append(light.Bytes(), scoped...)
	})
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(chromaCSSB)
}
