package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxJSONBody = 1 << 20

// clientIP returns the visitor's address. Proxy headers are only trusted when configured,
// because otherwise anyone could forge them to dodge the login lockout.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); net.ParseIP(ip) != nil {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func userAgent(r *http.Request) string { return truncate(r.UserAgent(), 200) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

var errBadJSON = errors.New("request body must be valid JSON")

func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errBadJSON
	}
	return nil
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// cleanName makes an uploaded file name safe to store and to put in Content-Disposition:
// base name only, no control characters, bounded length.
func cleanName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "file"
	}
	return truncate(name, 200)
}

// cleanMIME accepts a plain type/subtype token and falls back to octet-stream.
func cleanMIME(m string) string {
	m = strings.ToLower(strings.TrimSpace(strings.Split(m, ";")[0]))
	if len(m) == 0 || len(m) > 100 || strings.Count(m, "/") != 1 {
		return "application/octet-stream"
	}
	for _, c := range m {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("/.+-_", c)) {
			return "application/octet-stream"
		}
	}
	return m
}
