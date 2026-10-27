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
	}
	return cfg, cfg.Validate()
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
	}
	return nil
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
