package server

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"cpshare/internal/config"
)

type liveOpen struct {
	ID string `json:"id"`
}

func (e *env) openLive() string {
	e.t.Helper()
	r := e.api("POST", "/api/live", map[string]any{"device": "Firefox on Linux", "name": "holiday", "files": 3, "bytes": 5 << 30, "folder": true})
	if r.status != http.StatusCreated {
		e.t.Fatalf("open live: %d %s", r.status, r.body)
	}
	var o liveOpen
	r.json(e.t, &o)
	return o.ID
}

// stream opens the event stream of one side and returns its data lines.
func (e *env) stream(id, role string) (<-chan string, func()) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.http.URL+"/api/live/"+id+"/events?role="+role, nil)
	resp, err := e.owner.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		e.t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") || strings.HasPrefix(sc.Text(), "event: ") {
				lines <- sc.Text()
			}
		}
	}()
	return lines, func() { cancel(); resp.Body.Close() }
}

func next(t *testing.T, c <-chan string) string {
	t.Helper()
	select {
	case l, ok := <-c:
		if !ok {
			t.Fatal("stream ended")
		}
		return l
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	return ""
}

func TestLivePageNeedsLogin(t *testing.T) {
	e := newEnv(t)
	if r := e.get(e.client(), "/live"); r.status != http.StatusSeeOther {
		t.Fatalf("anonymous live page: %d", r.status)
	}
	if r := e.get(e.client(), "/api/live"); r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous live API: %d", r.status)
	}
	r := e.get(e.owner, "/live")
	if r.status != http.StatusOK {
		t.Fatalf("live page: %d", r.status)
	}
	mustContain(t, r.body, `data-ice="[{&#34;urls&#34;:&#34;stun:stun.example.org:3478&#34;}]"`)
}

func TestLiveHandshakeIsRelayed(t *testing.T) {
	e := newEnv(t)
	id := e.openLive()

	var list struct {
		Transfers []struct {
			ID    string `json:"id"`
			Offer struct {
				Name   string `json:"name"`
				Bytes  int64  `json:"bytes"`
				Folder bool   `json:"folder"`
			} `json:"offer"`
		} `json:"transfers"`
	}
	e.api("GET", "/api/live", nil).json(t, &list)
	if len(list.Transfers) != 1 || list.Transfers[0].ID != id || list.Transfers[0].Offer.Bytes != 5<<30 || !list.Transfers[0].Offer.Folder {
		t.Fatalf("waiting list: %+v", list)
	}

	snd, stopS := e.stream(id, "sender")
	defer stopS()
	if r := e.api("POST", "/api/live/"+id+"/join", nil); r.status != http.StatusNoContent {
		t.Fatalf("join: %d %s", r.status, r.body)
	}
	if got := next(t, snd); got != `data: {"type":"joined"}` {
		t.Fatalf("sender saw %q", got)
	}
	if r := e.api("POST", "/api/live/"+id+"/join", nil); r.status != http.StatusConflict {
		t.Fatalf("second join: %d", r.status)
	}

	rcv, stopR := e.stream(id, "receiver")
	defer stopR()
	if r := e.api("POST", "/api/live/"+id+"/signal?role=sender", map[string]any{"type": "offer", "sdp": "v=0\r\nline2"}); r.status != http.StatusNoContent {
		t.Fatalf("signal: %d %s", r.status, r.body)
	}
	if got := next(t, rcv); got != `data: {"sdp":"v=0\r\nline2","type":"offer"}` {
		t.Fatalf("receiver saw %q", got)
	}
	if r := e.api("POST", "/api/live/"+id+"/signal?role=receiver", map[string]any{"type": "answer"}); r.status != http.StatusNoContent {
		t.Fatalf("answer: %d", r.status)
	}
	if got := next(t, snd); got != `data: {"type":"answer"}` {
		t.Fatalf("sender saw %q", got)
	}

	if r := e.api("DELETE", "/api/live/"+id, nil); r.status != http.StatusNoContent {
		t.Fatalf("close: %d", r.status)
	}
	if got := next(t, rcv); got != "event: end" {
		t.Fatalf("receiver should be told the transfer ended, got %q", got)
	}
}

func TestLiveValidation(t *testing.T) {
	e := newEnv(t)
	bad := []map[string]any{
		{"device": "x", "name": "a", "files": 0, "bytes": 1},
		{"device": "x", "name": "a", "files": 1, "bytes": -1},
		{"device": "x", "name": "a", "files": 1, "bytes": 1, "extra": true},
	}
	for _, b := range bad {
		if r := e.api("POST", "/api/live", b); r.status != http.StatusBadRequest {
			t.Fatalf("%v: %d", b, r.status)
		}
	}
	id := e.openLive()
	if r := e.api("POST", "/api/live/"+id+"/signal?role=boss", map[string]any{}); r.status != http.StatusBadRequest {
		t.Fatalf("bad role: %d", r.status)
	}
	if r := e.api("POST", "/api/live/"+id+"/signal?role=sender", map[string]any{}); r.status != http.StatusConflict {
		t.Fatalf("signal before join: %d", r.status)
	}
	_ = e.api("POST", "/api/live/"+id+"/join", nil)
	r := e.do(e.owner, "POST", "/api/live/"+id+"/signal?role=sender", strings.NewReader("not json"),
		map[string]string{"X-CSRF-Token": e.csrf})
	if r.status != http.StatusBadRequest {
		t.Fatalf("invalid JSON: %d", r.status)
	}
	huge := `{"x":"` + strings.Repeat("a", 70<<10) + `"}`
	r = e.do(e.owner, "POST", "/api/live/"+id+"/signal?role=sender", strings.NewReader(huge),
		map[string]string{"X-CSRF-Token": e.csrf})
	if r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge signal: %d", r.status)
	}
	if r := e.api("POST", "/api/live/AAAAAAAAAAAAAAAAAAAAAA/join", nil); r.status != http.StatusNotFound {
		t.Fatalf("unknown id: %d", r.status)
	}
	if r := e.get(e.owner, "/api/live/bad/events?role=sender"); r.status != http.StatusNotFound {
		t.Fatalf("malformed id: %d", r.status)
	}
}

func TestLiveWritesNeedCSRF(t *testing.T) {
	e := newEnv(t)
	r := e.do(e.owner, "POST", "/api/live", strings.NewReader(`{"device":"x","name":"a","files":1,"bytes":1}`), nil)
	if r.status != http.StatusForbidden {
		t.Fatalf("live open without CSRF: %d", r.status)
	}
}

func TestLiveICEConfig(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.LiveSTUN = nil })
	mustContain(t, e.get(e.owner, "/live").body, `data-ice="[]"`)
}
