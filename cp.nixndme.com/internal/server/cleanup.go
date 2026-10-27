package server

import (
	"context"
	"io/fs"
	"path/filepath"
	"time"

	"cpshare/internal/ids"
	"cpshare/internal/vault"
)

// RunCleanup destroys due content on the configured interval until ctx is cancelled.
// It runs once immediately, so anything that expired while the Pi was off goes at boot.
func (s *Server) RunCleanup(ctx context.Context) {
	s.CleanupOnce()
	s.sweepOrphans()
	for {
		interval := time.Duration(defaultSettings().CleanupInterval) * time.Second
		if c, err := LoadSettings(s.st); err == nil {
			interval = time.Duration(c.CleanupInterval) * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.kick:
			timer.Stop()
		case <-timer.C:
		}
		s.CleanupOnce()
	}
}

// CleanupOnce ends due shares, drops abandoned uploads, expired sessions and old history.
func (s *Server) CleanupOnce() {
	settings, err := LoadSettings(s.st)
	if err != nil {
		s.log.Error("cleanup: settings", "err", err)
		return
	}
	due, err := s.st.DueForCleanup()
	if err != nil {
		s.log.Error("cleanup: due shares", "err", err)
		return
	}
	for _, d := range due {
		blobs, err := s.st.EndShare(d.PK, d.Reason, settings.KeepHistory)
		if err != nil {
			s.log.Error("cleanup: end share", "err", err)
			continue
		}
		s.deleteBlobs(blobs)
	}
	if len(due) > 0 {
		s.log.Info("cleanup: shares ended", "count", len(due))
	}
	s.dropStaleUploads()
	if err := s.st.PurgeExpired(); err != nil {
		s.log.Error("cleanup: sessions", "err", err)
	}
	if settings.HistoryDays > 0 {
		cutoff := s.st.Now() - settings.HistoryDays*24*3600
		if _, err := s.st.PurgeHistory(cutoff); err != nil {
			s.log.Error("cleanup: history", "err", err)
		}
	}
}

func (s *Server) dropStaleUploads() {
	stale, err := s.st.StaleUploads(s.st.Now() - int64(staleUploadAge.Seconds()))
	if err != nil {
		s.log.Error("cleanup: stale uploads", "err", err)
		return
	}
	for _, id := range stale {
		if err := s.vault.Delete(id); err != nil {
			s.log.Error("cleanup: delete upload blob", "err", err)
			continue
		}
		if err := s.st.DeleteUpload(id); err != nil {
			s.log.Error("cleanup: delete upload", "err", err)
		}
		s.uploadMu.Delete(id)
	}
}

// sweepOrphans deletes blob files no row refers to, e.g. after a power cut between the
// database commit and the file removal.
func (s *Server) sweepOrphans() {
	root := filepath.Join(s.cfg.DataDir, "blobs")
	removed := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !ids.Valid(d.Name(), vault.BlobIDLength) {
			return nil
		}
		inUse, err := s.st.BlobInUse(d.Name())
		if err != nil || inUse {
			return nil
		}
		if err := s.vault.Delete(d.Name()); err == nil {
			removed++
		}
		return nil
	})
	if err != nil {
		s.log.Error("cleanup: orphan sweep", "err", err)
	}
	if removed > 0 {
		s.log.Info("cleanup: orphan blobs removed", "count", removed)
	}
}
