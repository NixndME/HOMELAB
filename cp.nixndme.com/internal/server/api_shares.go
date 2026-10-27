package server

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"cpshare/internal/auth"
	"cpshare/internal/ids"
	"cpshare/internal/store"
)

const (
	maxTitle    = 200
	maxLinkURL  = 4096
	maxLinks    = 500
	maxDownload = 1_000_000
	logLimit    = 200
)

var languageRe = regexp.MustCompile(`^[a-z0-9+#._-]{0,40}$`)

type fileJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
}

type shareJSON struct {
	Number        int64      `json:"number"` // short, human-friendly: "Paste #12"
	Slug          string     `json:"slug"`
	Kind          string     `json:"kind"`
	Title         string     `json:"title"`
	Language      string     `json:"language"`
	LinkURL       string     `json:"link_url,omitempty"` // first link, kept for simple clients
	Links         []string   `json:"links"`
	URL           string     `json:"url"`
	CreatedAt     int64      `json:"created_at"`
	ExpiresAt     int64      `json:"expires_at"`
	Public        bool       `json:"public"`
	HasPassword   bool       `json:"has_password"`
	Burn          bool       `json:"burn"`
	MaxDownloads  int64      `json:"max_downloads"`
	Downloads     int64      `json:"downloads"`
	Views         int64      `json:"views"`
	ConsumedUntil int64      `json:"consumed_until"`
	Size          int64      `json:"size"`
	Active        bool       `json:"active"`
	EndedAt       int64      `json:"ended_at"`
	EndReason     string     `json:"end_reason"`
	Text          *fileJSON  `json:"text"`  // the pasted text, if any
	Files         []fileJSON `json:"files"` // attached files only
}

func (s *Server) toJSON(sh store.Share) shareJSON {
	out := shareJSON{
		Number: sh.PK, Slug: sh.Slug, Kind: sh.Kind, Title: sh.Title, Language: sh.Language, Links: sh.Links(),
		URL: s.cfg.BaseURL + "/s/" + sh.Slug, CreatedAt: sh.CreatedAt, ExpiresAt: sh.ExpiresAt,
		Public: sh.Public, HasPassword: sh.PassHash != "", Burn: sh.Burn, MaxDownloads: sh.MaxDownloads,
		Downloads: sh.Downloads, Views: sh.Views, ConsumedUntil: sh.ConsumedUntil, Size: sh.Size,
		Active: sh.Available(s.st.Now()), EndedAt: sh.EndedAt, EndReason: sh.EndReason, Files: []fileJSON{},
	}
	if out.Links == nil {
		out.Links = []string{}
	} else {
		out.LinkURL = out.Links[0]
	}
	if t, ok := sh.Text(); ok {
		out.Text = &fileJSON{ID: t.ID, Name: t.Name, MIME: t.MIME, Size: t.Size}
	}
	for _, f := range sh.Attachments() {
		out.Files = append(out.Files, fileJSON{ID: f.ID, Name: f.Name, MIME: f.MIME, Size: f.Size})
	}
	return out
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if status != "active" && status != "ended" {
		status = ""
	}
	shares, err := s.st.ListShares(store.ListFilter{Status: status, Query: truncate(q.Get("q"), 200)})
	if err != nil {
		s.internal(w, "list shares", err)
		return
	}
	out := make([]shareJSON, 0, len(shares))
	for _, sh := range shares {
		out = append(out, s.toJSON(sh))
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": out})
}

type createRequest struct {
	Kind         string   `json:"kind"`        // optional; older clients send text, file or link
	TextUpload   string   `json:"text_upload"` // upload id of the pasted text
	Title        string   `json:"title"`
	Language     string   `json:"language"`
	LinkURL      string   `json:"link_url"` // one link, or several separated by newlines
	Links        []string `json:"links"`
	Uploads      []string `json:"uploads"`
	ExpiresIn    int64    `json:"expires_in"` // seconds; 0 = default from settings
	Public       *bool    `json:"public"`     // nil = default from settings
	Password     string   `json:"password"`
	Burn         bool     `json:"burn"`
	MaxDownloads int64    `json:"max_downloads"`
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	settings, err := LoadSettings(s.st)
	if err != nil {
		s.internal(w, "load settings", err)
		return
	}
	sh, msg := s.buildShare(req, settings)
	if msg != "" {
		apiError(w, http.StatusBadRequest, msg)
		return
	}
	if req.Password != "" {
		if sh.PassHash, err = auth.HashPassword(req.Password); err != nil {
			s.internal(w, "hash share password", err)
			return
		}
	}
	if _, err := s.st.CreateShare(sh); err != nil {
		s.internal(w, "create share", err)
		return
	}
	created, err := s.st.ShareBySlug(sh.Slug) // reload for the file ids
	if err != nil {
		s.internal(w, "reload share", err)
		return
	}
	for _, id := range req.Uploads {
		s.uploadMu.Delete(id)
	}
	writeJSON(w, http.StatusCreated, s.toJSON(created))
}

// buildShare validates a create request; a non-empty message means it was rejected.
func (s *Server) buildShare(req createRequest, settings Settings) (store.Share, string) {
	now := s.st.Now()
	expiresIn := req.ExpiresIn
	if expiresIn == 0 {
		expiresIn = settings.DefaultExpiry
	}
	public := settings.DefaultPublic
	if req.Public != nil {
		public = *req.Public
	}
	sh := store.Share{
		Slug: ids.Share(), Kind: req.Kind, Title: truncate(strings.TrimSpace(req.Title), maxTitle),
		Language: strings.ToLower(strings.TrimSpace(req.Language)), CreatedAt: now, ExpiresAt: now + expiresIn,
		Public: public, Burn: req.Burn, MaxDownloads: req.MaxDownloads,
	}
	switch {
	case expiresIn < minExpiry || expiresIn > maxExpiry:
		return sh, "expiry must be between 1 minute and 365 days"
	case req.MaxDownloads < 0 || req.MaxDownloads > maxDownload:
		return sh, "max downloads must be 0 (unlimited) or a positive number"
	case !languageRe.MatchString(sh.Language):
		return sh, "language must be a short name like yaml or go"
	case len(req.Password) > 256:
		return sh, "share password is too long"
	}
	return s.attachParts(sh, req)
}

// attachParts adds the text, files and links of a request. Any combination is allowed, as
// long as there is at least one part. Kind is set to the main part: text, then files, then links.
func (s *Server) attachParts(sh store.Share, req createRequest) (store.Share, string) {
	textUpload, files := req.TextUpload, req.Uploads
	switch req.Kind {
	case "", store.KindFile, store.KindLink:
	case store.KindText: // older clients put the text upload in uploads
		if textUpload == "" && len(files) == 1 {
			textUpload, files = files[0], nil
		}
	default:
		return sh, "kind must be text, file or link"
	}
	if len(files) > s.cfg.MaxFiles {
		return sh, "a share holds at most " + strconv.Itoa(s.cfg.MaxFiles) + " files"
	}
	rawLinks := append(strings.Split(req.LinkURL, "\n"), req.Links...)
	if strings.TrimSpace(strings.Join(rawLinks, "")) != "" {
		links, msg := cleanLinks(rawLinks)
		if msg != "" {
			return sh, msg
		}
		sh.LinkURL = strings.Join(links, "\n")
	}
	if textUpload == "" && len(files) == 0 && sh.LinkURL == "" {
		return sh, "add some text, a file or a link"
	}
	var msg string
	if textUpload != "" {
		if sh, msg = s.attachUploads(sh, []string{textUpload}, store.RoleText); msg != "" {
			return sh, msg
		}
	}
	if sh, msg = s.attachUploads(sh, files, store.RoleFile); msg != "" {
		return sh, msg
	}
	switch {
	case textUpload != "":
		sh.Kind = store.KindText
	case len(files) > 0:
		sh.Kind = store.KindFile
	default:
		sh.Kind = store.KindLink
	}
	return sh, ""
}

// cleanLinks validates every non-empty line as an http(s) URL and keeps their order.
func cleanLinks(raw []string) ([]string, string) {
	var out []string
	for _, l := range raw {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		u, err := url.Parse(l)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(l) > maxLinkURL {
			return nil, "every link must be a full http:// or https:// URL: " + truncate(l, 80)
		}
		out = append(out, u.String())
	}
	switch {
	case len(out) == 0:
		return nil, "add at least one link"
	case len(out) > maxLinks:
		return nil, "a link share holds at most " + strconv.Itoa(maxLinks) + " links"
	}
	return out, ""
}

func (s *Server) attachUploads(sh store.Share, uploadIDs []string, role string) (store.Share, string) {
	seen := map[string]bool{}
	for _, f := range sh.Files {
		seen[f.BlobID] = true
	}
	for _, id := range uploadIDs {
		if seen[id] {
			return sh, "the same upload was listed twice"
		}
		seen[id] = true
		u, err := s.st.UploadByID(id)
		if err != nil {
			return sh, "an upload was not found or has expired; please upload again"
		}
		if !u.Complete() {
			return sh, "an upload is not finished yet"
		}
		mime, name := u.MIME, u.Name
		if role == store.RoleText {
			mime, name = "text/plain; charset=utf-8", "paste.txt"
		}
		sh.Files = append(sh.Files, store.File{BlobID: u.BlobID, Name: name, MIME: mime, Size: u.Declared, Role: role})
		sh.Size += u.Declared
	}
	return sh, ""
}

// ownedShare loads a share by the slug in the path for an owner API call.
func (s *Server) ownedShare(w http.ResponseWriter, r *http.Request) (store.Share, bool) {
	slug := r.PathValue("slug")
	if !ids.Valid(slug, ids.ShareLength) {
		apiError(w, http.StatusNotFound, "share not found")
		return store.Share{}, false
	}
	sh, err := s.st.ShareBySlug(slug)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "share not found")
		return store.Share{}, false
	} else if err != nil {
		s.internal(w, "load share", err)
		return store.Share{}, false
	}
	return sh, true
}

type updateRequest struct {
	Title        *string `json:"title"` // "" = back to the automatic name
	Public       *bool   `json:"public"`
	ExpiresIn    *int64  `json:"expires_in"` // new expiry, counted from now
	Password     *string `json:"password"`   // "" clears it
	MaxDownloads *int64  `json:"max_downloads"`
}

func (s *Server) updateShare(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.ownedShare(w, r)
	if !ok {
		return
	}
	if sh.Ended() {
		apiError(w, http.StatusGone, "this share has already ended")
		return
	}
	var req updateRequest
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg, err := s.applyUpdate(sh, req); msg != "" {
		apiError(w, http.StatusBadRequest, msg)
		return
	} else if err != nil {
		s.internal(w, "update share", err)
		return
	}
	updated, err := s.st.ShareBySlug(sh.Slug)
	if err != nil {
		s.internal(w, "reload share", err)
		return
	}
	writeJSON(w, http.StatusOK, s.toJSON(updated))
}

func (s *Server) applyUpdate(sh store.Share, req updateRequest) (string, error) {
	if req.Title != nil {
		if err := s.st.SetTitle(sh.PK, truncate(strings.TrimSpace(*req.Title), maxTitle)); err != nil {
			return "", err
		}
	}
	if req.ExpiresIn != nil {
		if *req.ExpiresIn < minExpiry || *req.ExpiresIn > maxExpiry {
			return "expiry must be between 1 minute and 365 days", nil
		}
		if err := s.st.SetExpiry(sh.PK, s.st.Now()+*req.ExpiresIn); err != nil {
			return "", err
		}
	}
	if req.Public != nil {
		if err := s.st.SetPublic(sh.PK, *req.Public); err != nil {
			return "", err
		}
	}
	if req.MaxDownloads != nil {
		if *req.MaxDownloads < 0 || *req.MaxDownloads > maxDownload {
			return "max downloads must be 0 (unlimited) or a positive number", nil
		}
		if err := s.st.SetMaxDownloads(sh.PK, *req.MaxDownloads); err != nil {
			return "", err
		}
	}
	if req.Password != nil {
		hash := ""
		if *req.Password != "" {
			if len(*req.Password) > 256 {
				return "share password is too long", nil
			}
			var err error
			if hash, err = auth.HashPassword(*req.Password); err != nil {
				return "", err
			}
		}
		if err := s.st.SetSharePassword(sh.PK, hash); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.ownedShare(w, r)
	if !ok {
		return
	}
	blobs, err := s.st.DeleteShare(sh.PK)
	if err != nil {
		s.internal(w, "delete share", err)
		return
	}
	s.deleteBlobs(blobs)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) regenerateShare(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.ownedShare(w, r)
	if !ok {
		return
	}
	if sh.Ended() {
		apiError(w, http.StatusGone, "this share has already ended")
		return
	}
	slug := ids.Share()
	if err := s.st.SetSlug(sh.PK, slug); err != nil {
		s.internal(w, "regenerate slug", err)
		return
	}
	updated, err := s.st.ShareBySlug(slug)
	if err != nil {
		s.internal(w, "reload share", err)
		return
	}
	writeJSON(w, http.StatusOK, s.toJSON(updated))
}

func (s *Server) shareLog(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.ownedShare(w, r)
	if !ok {
		return
	}
	entries, err := s.st.AccessLog(sh.PK, logLimit)
	if err != nil {
		s.internal(w, "access log", err)
		return
	}
	type entry struct {
		At     int64  `json:"at"`
		IP     string `json:"ip"`
		UA     string `json:"ua"`
		Action string `json:"action"`
		OK     bool   `json:"ok"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, entry{At: e.At, IP: e.IP, UA: e.UA, Action: e.Action, OK: e.OK})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// deleteBlobs removes vault files; failures are logged and retried by the orphan sweep.
func (s *Server) deleteBlobs(blobs []string) {
	for _, b := range blobs {
		if err := s.vault.Delete(b); err != nil {
			s.log.Error("delete blob", "err", err)
		}
	}
}
