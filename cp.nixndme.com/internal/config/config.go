// Package config reads the runtime configuration from environment variables.
// Nothing site-specific is hardcoded: every value has a safe default or comes from .env.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultAddr       = ":8080"
	defaultDataDir    = "/data"
	defaultMaxFileMB  = 100
	defaultChunkMB    = 8 // stays well under Cloudflare's 100 MB request body limit
	defaultStorageMB  = 5120
	defaultMaxFiles   = 20
	minPasswordLength = 8
	// A public STUN server only tells each browser its own address; file data never goes there.
	defaultLiveSTUN = "stun:stun.cloudflare.com:3478"
	// Start page searches that are not the secret word go here.
	defaultSearchURL = "https://duckduckgo.com/?q={QUERY}"
	// The relay secret signs every relay login; anything shorter is guessable offline.
	minTURNSecretLength = 32
)

// Config is the immutable runtime configuration.
type Config struct {
	Addr          string
	DataDir       string
	KeyFile       string
	BaseURL       string // public origin used in share links and QR codes, e.g. https://cp.example.com
	AdminUser     string // only used on first boot, when no user exists yet
	AdminPassword string // only used on first boot
	TrustProxy    bool   // trust CF-Connecting-IP / X-Forwarded-For for client IPs
	SecureCookies bool
	MaxFileBytes  int64
	ChunkBytes    int64
	StorageBytes  int64 // hard cap on bytes stored on disk
	MaxFiles      int   // max files in one share
	// LiveSTUN lists the STUN servers Live Transfer browsers may use to find each other when
	// local addresses alone do not work. Empty means local network candidates only.
	LiveSTUN []string
	// LiveTURN lists relay URLs (e.g. coturn on the Pi) used when two devices cannot connect
	// directly, typically because a device firewall drops incoming traffic. The data stays
	// end-to-end encrypted (DTLS) through the relay. Empty means no relay.
	LiveTURN []string
	// LiveTURNSecret is the static-auth-secret shared with the relay; cp derives short-lived
	// relay logins from it, so the relay is never open to anyone without a cp session.
	LiveTURNSecret string
	// HomeUpstream is the start page server (Glance) proxied under /home, e.g.
	// http://home:8080. Empty means a built-in plain search page instead.
	HomeUpstream string
	// SearchURL is where start page searches go; {QUERY} is replaced by the search.
	SearchURL string
}

// MinPasswordLength is the shortest password accepted for the login user.
func MinPasswordLength() int { return minPasswordLength }

// Load builds a Config from the environment and validates it.
func Load() (Config, error) {
	dataDir := env("CP_DATA_DIR", defaultDataDir)
	cfg := Config{
		Addr:          env("CP_ADDR", defaultAddr),
		DataDir:       dataDir,
		KeyFile:       env("CP_KEY_FILE", filepath.Join(dataDir, "secret.key")),
		BaseURL:       strings.TrimRight(env("CP_BASE_URL", "http://localhost:8080"), "/"),
		AdminUser:     os.Getenv("CP_ADMIN_USER"),
		AdminPassword: os.Getenv("CP_ADMIN_PASSWORD"),
		TrustProxy:    envBool("CP_TRUST_PROXY", false),
		SecureCookies: envBool("CP_SECURE_COOKIES", false),
		MaxFileBytes:  envInt("CP_MAX_FILE_MB", defaultMaxFileMB) << 20,
		ChunkBytes:    envInt("CP_CHUNK_MB", defaultChunkMB) << 20,
		StorageBytes:  envInt("CP_STORAGE_CAP_MB", defaultStorageMB) << 20,
		MaxFiles:      int(envInt("CP_MAX_FILES_PER_SHARE", defaultMaxFiles)),
		LiveSTUN:      envList("CP_LIVE_STUN", defaultLiveSTUN),
		LiveTURN:      envList("CP_LIVE_TURN", ""),
		// Not trimmed by env(): a secret is used exactly as written, like coturn reads it.
		LiveTURNSecret: os.Getenv("CP_LIVE_TURN_SECRET"),
		HomeUpstream:   strings.TrimRight(os.Getenv("CP_HOME_UPSTREAM"), "/"),
		SearchURL:      env("CP_SEARCH_URL", defaultSearchURL),
	}
	return cfg, cfg.Validate()
}

// envList reads a comma-separated list. Unset uses def; "none" (or set but empty) means no entries.
func envList(key, def string) []string {
	v, ok := os.LookupEnv(key)
	if !ok {
		v = def
	}
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" && !strings.EqualFold(item, "none") {
			out = append(out, item)
		}
	}
	return out
}

// Validate rejects configurations that cannot work.
func (c Config) Validate() error {
	switch {
	case c.MaxFileBytes <= 0, c.ChunkBytes <= 0, c.StorageBytes <= 0, c.MaxFiles <= 0:
		return fmt.Errorf("size limits must be positive")
	case c.ChunkBytes > 64<<20:
		return fmt.Errorf("CP_CHUNK_MB must be at most 64 (Cloudflare caps request bodies at 100 MB)")
	case !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://"):
		return fmt.Errorf("CP_BASE_URL must start with http:// or https://")
	case c.HomeUpstream != "" && !strings.HasPrefix(c.HomeUpstream, "http://") && !strings.HasPrefix(c.HomeUpstream, "https://"):
		return fmt.Errorf("CP_HOME_UPSTREAM must start with http:// or https://")
	case !strings.HasPrefix(c.SearchURL, "https://") || !strings.Contains(c.SearchURL, "{QUERY}"):
		return fmt.Errorf("CP_SEARCH_URL must be an https:// URL containing {QUERY}")
	}
	for _, u := range c.LiveSTUN {
		if !strings.HasPrefix(u, "stun:") && !strings.HasPrefix(u, "stuns:") {
			return fmt.Errorf("CP_LIVE_STUN entries must start with stun: or stuns: (got %q)", u)
		}
	}
	return c.validateTURN()
}

func (c Config) validateTURN() error {
	for _, u := range c.LiveTURN {
		if !strings.HasPrefix(u, "turn:") && !strings.HasPrefix(u, "turns:") {
			return fmt.Errorf("CP_LIVE_TURN entries must start with turn: or turns: (got %q)", u)
		}
	}
	switch {
	case len(c.LiveTURN) > 0 && len(c.LiveTURNSecret) < minTURNSecretLength:
		return fmt.Errorf("CP_LIVE_TURN needs CP_LIVE_TURN_SECRET of at least %d characters (openssl rand -hex 32)",
			minTURNSecretLength)
	case len(c.LiveTURN) == 0 && c.LiveTURNSecret != "":
		return fmt.Errorf("CP_LIVE_TURN_SECRET is set but CP_LIVE_TURN is empty")
	case strings.IndexFunc(c.LiveTURNSecret, notAlnum) >= 0:
		// The coturn image's entrypoint passes each argument through eval, so shell
		// characters in the secret would silently change it on the relay side.
		return fmt.Errorf("CP_LIVE_TURN_SECRET may only contain letters and digits (openssl rand -hex 32)")
	}
	return nil
}

func notAlnum(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return def
	}
	return v
}

func envInt(key string, def int64) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
	if err != nil {
		return def
	}
	return v
}
