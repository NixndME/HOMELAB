package server

import (
	"fmt"
	"strconv"
	"time"

	"cpshare/internal/store"
)

// Settings are the owner-editable preferences stored in the settings table.
type Settings struct {
	DefaultExpiry   int64 `json:"default_expiry"`   // seconds
	CleanupInterval int64 `json:"cleanup_interval"` // seconds
	PublicSharing   bool  `json:"public_sharing"`   // global switch for public links and QR
	DefaultPublic   bool  `json:"default_public"`   // new shares get a public link by default
	KeepHistory     bool  `json:"keep_history"`     // keep metadata of ended shares on the dashboard
	HistoryDays     int64 `json:"history_days"`     // drop that metadata after N days (0 = keep)
}

// Allowed ranges.
const (
	minExpiry          = 60
	maxExpiry          = 365 * 24 * 3600
	minCleanupInterval = 30
	maxCleanupInterval = 24 * 3600
	maxHistoryDays     = 3650
)

func defaultSettings() Settings {
	return Settings{
		DefaultExpiry:   int64((24 * time.Hour).Seconds()),
		CleanupInterval: int64((5 * time.Minute).Seconds()),
		PublicSharing:   true,
		DefaultPublic:   false,
		KeepHistory:     true,
		HistoryDays:     30,
	}
}

// Validate checks every field is in range.
func (c Settings) Validate() error {
	switch {
	case c.DefaultExpiry < minExpiry || c.DefaultExpiry > maxExpiry:
		return fmt.Errorf("default expiry must be between 1 minute and 365 days")
	case c.CleanupInterval < minCleanupInterval || c.CleanupInterval > maxCleanupInterval:
		return fmt.Errorf("cleanup interval must be between 30 seconds and 24 hours")
	case c.HistoryDays < 0 || c.HistoryDays > maxHistoryDays:
		return fmt.Errorf("history days must be between 0 and %d", maxHistoryDays)
	}
	return nil
}

// LoadSettings reads settings, filling anything unset with defaults.
func LoadSettings(st *store.Store) (Settings, error) {
	raw, err := st.Settings()
	if err != nil {
		return Settings{}, err
	}
	c := defaultSettings()
	readInt(raw, "default_expiry", &c.DefaultExpiry)
	readInt(raw, "cleanup_interval", &c.CleanupInterval)
	readInt(raw, "history_days", &c.HistoryDays)
	readBool(raw, "public_sharing", &c.PublicSharing)
	readBool(raw, "default_public", &c.DefaultPublic)
	readBool(raw, "keep_history", &c.KeepHistory)
	return c, nil
}

// SaveSettings validates and stores settings.
func SaveSettings(st *store.Store, c Settings) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return st.PutSettings(map[string]string{
		"default_expiry":   store.FormatInt(c.DefaultExpiry),
		"cleanup_interval": store.FormatInt(c.CleanupInterval),
		"history_days":     store.FormatInt(c.HistoryDays),
		"public_sharing":   strconv.FormatBool(c.PublicSharing),
		"default_public":   strconv.FormatBool(c.DefaultPublic),
		"keep_history":     strconv.FormatBool(c.KeepHistory),
	})
}

func readInt(raw map[string]string, key string, dst *int64) {
	if v, err := strconv.ParseInt(raw[key], 10, 64); err == nil {
		*dst = v
	}
}

func readBool(raw map[string]string, key string, dst *bool) {
	if v, err := strconv.ParseBool(raw[key]); err == nil {
		*dst = v
	}
}
