package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"cpshare/internal/ids"
	"cpshare/internal/live"
)

// Live Transfer: the server only introduces the two browsers; file bytes go peer to peer.
const (
	maxLiveSignal   = 32 << 10 // an SDP is a few KB, an ICE candidate a few hundred bytes
	maxLiveFiles    = 1_000_000
	maxLiveBytes    = 1 << 50
	livePing        = 20 * time.Second // well under Cloudflare's 100 s idle cut-off
	liveSweepEvery  = 15 * time.Second
	liveSenderGrace = 45 * time.Second // covers an EventSource reconnect
	liveRoomTTL     = 24 * time.Hour
)

func newLiveHub() *live.Hub {
	return live.NewHub(live.Limits{MaxRooms: 8, MaxQueue: 64, MaxMessage: maxLiveSignal,
		SenderGrace: liveSenderGrace, RoomTTL: liveRoomTTL})
}

func (s *Server) liveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /live", s.requirePage(s.livePage))
	api := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.requireAPI(h)) }
	api("GET /api/live", s.listLive)
	api("POST /api/live", s.openLive)
	api("POST /api/live/{id}/join", s.joinLive)
	api("POST /api/live/{id}/signal", s.signalLive)
	api("GET /api/live/{id}/events", s.liveEvents)
	api("DELETE /api/live/{id}", s.closeLive)
}

func (s *Server) livePage(w http.ResponseWriter, r *http.Request) {
	servers := []map[string]string{}
	for _, u := range s.cfg.LiveSTUN {
		servers = append(servers, map[string]string{"urls": u})
	}
	ice, _ := json.Marshal(servers) // a slice of string maps always marshals
	s.render(w, http.StatusOK, "live", map[string]any{
		"CSRF": ownerFrom(r.Context()).CSRF, "Nav": "live", "ICE": string(ice),
	})
}

func (s *Server) listLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"transfers": s.live.Waiting()})
}

func (s *Server) openLive(w http.ResponseWriter, r *http.Request) {
	var req live.Offer
	if err := readJSON(w, r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Files < 1 || req.Files > maxLiveFiles || req.Bytes < 0 || req.Bytes > maxLiveBytes {
		apiError(w, http.StatusBadRequest, "files and bytes are out of range")
		return
	}
	req.Device = cleanLabel(req.Device, 80)
	req.Name = cleanName(req.Name)
	id, err := s.live.Open(req)
	if err != nil {
		apiError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) joinLive(w http.ResponseWriter, r *http.Request) {
	id, ok := liveID(w, r)
	if !ok {
		return
	}
	if err := s.live.Join(id); err != nil {
		liveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) signalLive(w http.ResponseWriter, r *http.Request) {
	id, ok := liveID(w, r)
	if !ok {
		return
	}
	role, ok := liveRole(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLiveSignal))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "signal message too large")
		return
	}
	// Compact to one line: each message must fit in a single SSE "data:" line.
	var msg bytes.Buffer
	if err := json.Compact(&msg, body); err != nil {
		apiError(w, http.StatusBadRequest, errBadJSON.Error())
		return
	}
	if err := s.live.Signal(id, role, msg.Bytes()); err != nil {
		liveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) closeLive(w http.ResponseWriter, r *http.Request) {
	id, ok := liveID(w, r)
	if !ok {
		return
	}
	s.live.Close(id)
	w.WriteHeader(http.StatusNoContent)
}

// liveEvents streams one side's handshake messages as Server-Sent Events until the transfer
// ends, another tab of the same side takes over, or the browser leaves.
func (s *Server) liveEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := liveID(w, r)
	if !ok {
		return
	}
	role, ok := liveRole(w, r)
	if !ok {
		return
	}
	sub, err := s.live.Subscribe(id, role)
	if err != nil {
		liveError(w, err)
		return
	}
	defer sub.Close()

	rc := http.NewResponseController(w)
	// The server's ReadTimeout would otherwise cancel this long-lived request after 10 minutes.
	_ = rc.SetReadDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// An error here only means the browser left; EventSource reconnects by itself if needed.
	_ = streamLive(r.Context(), w, rc, sub)
}

func streamLive(ctx context.Context, w io.Writer, rc *http.ResponseController, sub *live.Subscription) error {
	send := func(format string, args ...any) error {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := send(": connected\n\n"); err != nil {
		return err
	}
	ping := time.NewTicker(livePing)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ping.C:
			if err := send(": ping\n\n"); err != nil {
				return err
			}
		case msg, open := <-sub.C:
			if !open {
				return send("event: end\ndata: {}\n\n")
			}
			if err := send("data: %s\n\n", msg); err != nil {
				return err
			}
		}
	}
}

// RunLiveSweep drops abandoned live transfers until ctx is cancelled.
func (s *Server) RunLiveSweep(ctx context.Context) {
	t := time.NewTicker(liveSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.live.Sweep()
		}
	}
}

// CloseLive ends every live transfer, so open event streams return and shutdown is not held up.
func (s *Server) CloseLive() { s.live.CloseAll() }

func liveID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !ids.Valid(id, ids.ShareLength) {
		apiError(w, http.StatusNotFound, live.ErrNotFound.Error())
		return "", false
	}
	return id, true
}

func liveRole(w http.ResponseWriter, r *http.Request) (live.Role, bool) {
	switch r.URL.Query().Get("role") {
	case "sender":
		return live.Sender, true
	case "receiver":
		return live.Receiver, true
	}
	apiError(w, http.StatusBadRequest, "role must be sender or receiver")
	return 0, false
}

func liveError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, live.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, live.ErrTaken), errors.Is(err, live.ErrNoPeer):
		status = http.StatusConflict
	case errors.Is(err, live.ErrTooBig):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, live.ErrBusy):
		status = http.StatusServiceUnavailable
	}
	apiError(w, status, err.Error())
}

// cleanLabel keeps a short, printable, single-line label.
func cleanLabel(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" {
		s = "Unknown device"
	}
	return truncate(s, n)
}
