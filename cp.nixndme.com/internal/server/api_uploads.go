package server

import (
	"errors"
	"net/http"
	"strconv"

	"cpshare/internal/store"
	"cpshare/internal/vault"
)

// startUpload registers a chunked upload after checking the size limit and storage cap.
// Every file and every pasted text goes through this path, so a 100 MB file or a
// million-line paste never travels in one request (Cloudflare refuses bodies over 100 MB).
func (s *Server) startUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		MIME string `json:"mime"`
	}
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Size <= 0 || req.Size > s.cfg.MaxFileBytes {
		apiError(w, http.StatusRequestEntityTooLarge, "each file must be between 1 byte and "+humanBytes(s.cfg.MaxFileBytes))
		return
	}
	used, err := s.st.StoredBytes()
	if err != nil {
		s.internal(w, "stored bytes", err)
		return
	}
	if used+req.Size > s.cfg.StorageBytes {
		apiError(w, http.StatusInsufficientStorage, "storage is full ("+humanBytes(used)+" of "+
			humanBytes(s.cfg.StorageBytes)+" used); delete some shares first")
		return
	}
	u := store.Upload{BlobID: vault.NewID(), Name: cleanName(req.Name), MIME: cleanMIME(req.MIME), Declared: req.Size}
	if err := s.st.CreateUpload(u); err != nil {
		s.internal(w, "create upload", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": u.BlobID, "chunk_bytes": s.cfg.ChunkBytes})
}

// putChunk appends one chunk. Chunks must arrive in order (0, 1, 2…); a retried chunk that
// was already stored is acknowledged without being written twice.
func (s *Server) putChunk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	chunk, err := strconv.ParseInt(r.PathValue("chunk"), 10, 64)
	if err != nil || chunk < 0 {
		apiError(w, http.StatusBadRequest, "bad chunk index")
		return
	}
	unlock := s.lockUpload(id)
	defer unlock()
	u, err := s.st.UploadByID(id)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "upload not found or expired")
		return
	} else if err != nil {
		s.internal(w, "load upload", err)
		return
	}
	if chunk < u.NextChunk {
		writeJSON(w, http.StatusOK, map[string]any{"received": u.Received, "complete": u.Complete()})
		return
	}
	if chunk != u.NextChunk {
		apiError(w, http.StatusConflict, "expected chunk "+strconv.FormatInt(u.NextChunk, 10))
		return
	}
	remaining := u.Declared - u.Received
	if remaining <= 0 {
		apiError(w, http.StatusConflict, "upload is already complete")
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.cfg.ChunkBytes)
	records, n, err := s.vault.Append(u.BlobID, u.Records, body, min(remaining, s.cfg.ChunkBytes))
	if err != nil {
		status, msg := http.StatusBadRequest, "chunk could not be stored"
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) || errors.Is(err, vault.ErrTooLarge) {
			status, msg = http.StatusRequestEntityTooLarge, "chunk is larger than allowed"
		}
		s.log.Warn("append chunk", "err", err)
		apiError(w, status, msg)
		return
	}
	if err := s.st.AdvanceUpload(u.BlobID, chunk, records, n); err != nil {
		s.internal(w, "advance upload", err)
		return
	}
	received := u.Received + n
	writeJSON(w, http.StatusOK, map[string]any{"received": received, "complete": received == u.Declared})
}

func (s *Server) cancelUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	unlock := s.lockUpload(id)
	defer unlock()
	if _, err := s.st.UploadByID(id); err != nil {
		apiError(w, http.StatusNotFound, "upload not found")
		return
	}
	if err := s.vault.Delete(id); err != nil {
		s.internal(w, "delete blob", err)
		return
	}
	if err := s.st.DeleteUpload(id); err != nil {
		s.internal(w, "delete upload", err)
		return
	}
	s.uploadMu.Delete(id)
	w.WriteHeader(http.StatusNoContent)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "B"
}
