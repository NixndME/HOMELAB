package server

import (
	"archive/zip"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/skip2/go-qrcode"

	"cpshare/internal/auth"
	"cpshare/internal/ids"
	"cpshare/internal/store"
)

// access is what a request may do with a share.
type access int

const (
	accessNone   access = iota // unknown, ended, private or public links disabled: always a plain 404
	accessLocked               // needs the share password
	accessBurn                 // burn-after-reading, not yet revealed
	accessOK
)

// Grant cookies: one for a password unlock, one for a burn reveal. They are separate so a
// visitor who only unlocked the password cannot read a share someone else already revealed.
func unlockCookie(slug string) string { return "cpu_" + slug }
func burnCookie(slug string) string   { return "cpb_" + slug }

type viewer struct {
	share store.Share
	owner bool
	level access
}

// resolve decides what the requester may do. Visitors get exactly the same 404 for a share
// that never existed, has expired, is private, or while public sharing is switched off,
// so nothing can be learned by probing links.
func (s *Server) resolve(r *http.Request) (viewer, error) {
	slug := r.PathValue("slug")
	if !ids.Valid(slug, ids.ShareLength) {
		return viewer{}, nil
	}
	sh, err := s.st.ShareBySlug(slug)
	if errors.Is(err, store.ErrNotFound) {
		return viewer{}, nil
	} else if err != nil {
		return viewer{}, err
	}
	now := s.st.Now()
	if _, ok := s.currentUser(r); ok {
		if !sh.Available(now) {
			return viewer{share: sh, owner: true}, nil
		}
		return viewer{share: sh, owner: true, level: accessOK}, nil
	}
	settings, err := LoadSettings(s.st)
	if err != nil {
		return viewer{}, err
	}
	if !settings.PublicSharing || !sh.Public || !sh.Available(now) {
		return viewer{}, nil
	}
	v := viewer{share: sh, level: accessOK}
	if sh.PassHash != "" && !s.hasGrant(r, unlockCookie(slug), sh.PK) {
		v.level = accessLocked
		return v, nil
	}
	if sh.Burn && !s.hasGrant(r, burnCookie(slug), sh.PK) {
		if sh.ConsumedUntil != 0 {
			return viewer{}, nil // someone already revealed it
		}
		v.level = accessBurn
	}
	return v, nil
}

func (s *Server) hasGrant(r *http.Request, name string, pk int64) bool {
	c, err := r.Cookie(name)
	if err != nil || !ids.Valid(c.Value, ids.TokenLength) {
		return false
	}
	ok, err := s.st.GrantValid(auth.HashToken(c.Value), pk)
	return err == nil && ok
}

func (s *Server) issueGrant(w http.ResponseWriter, name string, pk int64, lifetime time.Duration) error {
	token := ids.Token()
	if err := s.st.CreateGrant(auth.HashToken(token), pk, s.now().Add(lifetime).Unix()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: "/", HttpOnly: true,
		Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: int(lifetime.Seconds())})
	return nil
}

func (s *Server) logVisit(r *http.Request, v viewer, action string, ok bool) {
	if v.owner {
		return
	}
	if err := s.st.LogAccess(v.share.PK, store.Access{IP: s.clientIP(r), UA: userAgent(r), Action: action, OK: ok}); err != nil {
		s.log.Error("access log", "err", err)
	}
}

func (s *Server) notFound(w http.ResponseWriter) {
	s.render(w, http.StatusNotFound, "gone", map[string]any{})
}

func (s *Server) viewShare(w http.ResponseWriter, r *http.Request) {
	v, err := s.resolve(r)
	if err != nil {
		s.log.Error("resolve share", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch v.level {
	case accessNone:
		if v.owner {
			s.render(w, http.StatusGone, "gone", map[string]any{"Ended": v.share})
			return
		}
		s.notFound(w)
	case accessLocked:
		s.render(w, http.StatusOK, "share", map[string]any{"Share": v.share, "Locked": true})
	case accessBurn:
		s.render(w, http.StatusOK, "share", map[string]any{"Share": v.share, "Burn": true})
	default:
		s.renderShare(w, r, v)
	}
}

func (s *Server) renderShare(w http.ResponseWriter, r *http.Request, v viewer) {
	data := map[string]any{"Share": v.share, "Owner": v.owner, "URL": s.cfg.BaseURL + "/s/" + v.share.Slug,
		"Embed":       r.URL.Query().Get("embed") == "1", // inside the dashboard's view popup: no header
		"Attachments": v.share.Attachments()}
	if text, ok := v.share.Text(); ok {
		data["Text"] = text
		p, err := s.preview(text, v.share.Language)
		if err != nil {
			s.log.Error("text preview", "err", err)
			http.Error(w, "this share could not be read", http.StatusInternalServerError)
			return
		}
		data["Preview"] = p
	}
	if !v.owner {
		if err := s.st.CountView(v.share.PK); err != nil {
			s.log.Error("count view", "err", err)
		}
		s.logVisit(r, v, "view", true)
	}
	s.render(w, http.StatusOK, "share", data)
}

func (s *Server) unlockShare(w http.ResponseWriter, r *http.Request) {
	v, err := s.resolve(r)
	if err != nil || v.level != accessLocked {
		http.Redirect(w, r, "/s/"+r.PathValue("slug"), http.StatusSeeOther)
		return
	}
	ip := s.clientIP(r)
	if locked, _ := s.unlock.Locked(ip); locked {
		s.render(w, http.StatusTooManyRequests, "share", map[string]any{"Share": v.share, "Locked": true,
			"Error": "Too many wrong passwords. Try again in 15 minutes."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if !auth.VerifyPassword(v.share.PassHash, r.PostFormValue("password")) {
		s.unlock.Fail(ip)
		s.logVisit(r, v, "password", false)
		s.render(w, http.StatusUnauthorized, "share", map[string]any{"Share": v.share, "Locked": true, "Error": "Wrong password."})
		return
	}
	s.unlock.Succeed(ip)
	s.logVisit(r, v, "password", true)
	if err := s.issueGrant(w, unlockCookie(v.share.Slug), v.share.PK, grantLifetime); err != nil {
		s.log.Error("issue grant", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/s/"+v.share.Slug, http.StatusSeeOther)
}

// revealShare opens a burn-after-reading share. Only the first visitor succeeds; they can read
// and download for burnWindow, after which cleanup destroys the content.
func (s *Server) revealShare(w http.ResponseWriter, r *http.Request) {
	v, err := s.resolve(r)
	if err != nil || v.level != accessBurn {
		http.Redirect(w, r, "/s/"+r.PathValue("slug"), http.StatusSeeOther)
		return
	}
	won, err := s.st.Consume(v.share.PK, s.now().Add(burnWindow).Unix())
	if err == nil && won {
		err = s.issueGrant(w, burnCookie(v.share.Slug), v.share.PK, burnWindow)
	}
	if err != nil {
		s.log.Error("reveal", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logVisit(r, v, "reveal", won)
	if !won {
		s.notFound(w)
		return
	}
	http.Redirect(w, r, "/s/"+v.share.Slug, http.StatusSeeOther)
}

// readable resolves the share for a content route and claims a download slot for visitors.
func (s *Server) readable(w http.ResponseWriter, r *http.Request, action string) (viewer, bool) {
	v, err := s.resolve(r)
	if err != nil {
		s.log.Error("resolve share", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return v, false
	}
	if v.level != accessOK {
		http.NotFound(w, r)
		return v, false
	}
	if !v.owner {
		took, err := s.st.TakeDownload(v.share.PK)
		if err != nil || !took {
			s.logVisit(r, v, action, false)
			http.NotFound(w, r)
			return v, false
		}
		s.logVisit(r, v, action, true)
		if v.share.MaxDownloads > 0 && v.share.Downloads+1 >= v.share.MaxDownloads {
			s.Kick() // last allowed download: let cleanup remove it right after
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	return v, true
}

// rawShare serves the full text (or the link target) as text/plain, handy for curl.
func (s *Server) rawShare(w http.ResponseWriter, r *http.Request) {
	v, ok := s.readable(w, r, "raw")
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if text, ok := v.share.Text(); ok {
		s.streamFile(w, text)
		return
	}
	if v.share.LinkURL != "" {
		io.WriteString(w, v.share.LinkURL+"\n")
		return
	}
	http.Error(w, "this share has no text or links", http.StatusNotFound)
}

// inlineSafe lists the only types ever rendered in the browser; everything else downloads.
var inlineSafe = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"application/pdf": true, "text/plain": true,
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	fileID, err := strconv.ParseInt(r.PathValue("file"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	v, ok := s.readable(w, r, "download")
	if !ok {
		return
	}
	for _, f := range v.share.Files {
		if f.ID != fileID {
			continue
		}
		base, _, _ := mime.ParseMediaType(f.MIME)
		disposition, ctype := "attachment", "application/octet-stream"
		if r.URL.Query().Get("inline") == "1" && inlineSafe[base] {
			disposition, ctype = "inline", f.MIME
			if base == "application/pdf" {
				w.Header().Del("Content-Security-Policy") // the browser's own PDF viewer refuses to run sandboxed
			} else {
				w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
			}
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": f.Name}))
		w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
		s.streamFile(w, f)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) streamFile(w http.ResponseWriter, f store.File) {
	rd, err := s.vault.Reader(f.BlobID, f.Size)
	if err != nil {
		s.log.Error("open blob", "err", err)
		http.Error(w, "file is missing", http.StatusInternalServerError)
		return
	}
	defer rd.Close()
	if _, err := io.Copy(w, rd); err != nil {
		s.log.Error("stream blob", "err", err) // headers are already sent; the client sees a cut transfer
	}
}

// zipShare streams every file of a share as one zip archive (stored, not compressed: the
// files are often already compressed and the Pi's CPU is better spent elsewhere).
func (s *Server) zipShare(w http.ResponseWriter, r *http.Request) {
	v, ok := s.readable(w, r, "zip")
	if !ok {
		return
	}
	if len(v.share.Files) == 0 {
		http.NotFound(w, r)
		return
	}
	name := "cp-" + v.share.Slug[:8] + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	zw := zip.NewWriter(w)
	used := map[string]int{}
	for _, f := range v.share.Files {
		entry := f.Name
		if n := used[entry]; n > 0 {
			entry = strconv.Itoa(n) + "-" + entry
		}
		used[f.Name]++
		hw, err := zw.CreateHeader(&zip.FileHeader{Name: entry, Method: zip.Store, Modified: time.Unix(v.share.CreatedAt, 0)})
		if err != nil {
			s.log.Error("zip entry", "err", err)
			return
		}
		rd, err := s.vault.Reader(f.BlobID, f.Size)
		if err != nil {
			s.log.Error("open blob", "err", err)
			return
		}
		_, err = io.Copy(hw, rd)
		rd.Close()
		if err != nil {
			s.log.Error("zip copy", "err", err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		s.log.Error("zip close", "err", err)
	}
}

// qrShare returns a PNG QR code of the share link.
func (s *Server) qrShare(w http.ResponseWriter, r *http.Request) {
	v, err := s.resolve(r)
	if err != nil || v.level == accessNone {
		http.NotFound(w, r)
		return
	}
	png, err := qrcode.Encode(s.cfg.BaseURL+"/s/"+v.share.Slug, qrcode.Medium, 320)
	if err != nil {
		s.log.Error("qr", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}
