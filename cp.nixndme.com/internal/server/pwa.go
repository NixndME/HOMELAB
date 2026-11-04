package server

import (
	"io/fs"
	"net/http"

	"cpshare/web"
)

// appRoutes serves what makes cp installable as an app (manifest, service worker), so the
// phone's share sheet can send text, links and files to it, plus the cpup upload script.
func (s *Server) appRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /manifest.webmanifest", embedded("static/manifest.webmanifest", "application/manifest+json", "public, max-age=3600"))
	// The worker must live at the root to control the whole site. no-cache: browsers then
	// check for a new version on every visit, so a release reaches installed apps quickly.
	mux.HandleFunc("GET /sw.js", embedded("static/sw.js", "text/javascript; charset=utf-8", "no-cache"))
	// The upload script only for the owner (logged in) or a token holder; others get the
	// same 404 as any unknown address, so it says nothing about what runs here.
	cpup := embedded("static/cpup.sh", "text/plain; charset=utf-8", "no-store")
	mux.HandleFunc("GET /cpup", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentUser(r); !ok {
			if _, ok := s.lookupToken(r); !ok {
				http.NotFound(w, r)
				return
			}
		}
		cpup(w, r)
	})
	// The service worker answers shares from the share sheet itself and keeps their files in
	// the browser. This only runs when it is missing (first launch before it installed):
	// nothing is stored, the owner just lands on the dashboard.
	mux.HandleFunc("POST /share-target", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/?shared=lost", http.StatusSeeOther)
	})
}

// embedded serves one file from the binary with a fixed type and cache policy.
func embedded(name, contentType, cache string) http.HandlerFunc {
	body, err := fs.ReadFile(web.FS, name)
	if err != nil {
		panic(err) // embedded at build time; cannot fail at runtime
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", cache)
		w.Write(body)
	}
}
