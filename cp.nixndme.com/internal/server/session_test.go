package server

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func (e *env) background(c *http.Client, path string) result {
	return e.do(c, "GET", path, nil, map[string]string{backgroundHeader: "1"})
}

type sessionList struct {
	Sessions []sessionJSON `json:"sessions"`
	Max      int           `json:"max"`
	Idle     int64         `json:"idle_seconds"`
}

func (e *env) sessions() sessionList {
	e.t.Helper()
	r := e.api("GET", "/api/account/sessions", nil)
	if r.status != http.StatusOK {
		e.t.Fatalf("list sessions: %d %s", r.status, r.body)
	}
	var l sessionList
	r.json(e.t, &l)
	return l
}

func TestIdleLoginEndsAfterDefaultFifteenMinutes(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(14 * time.Minute)
	if r := e.background(e.owner, "/api/shares"); r.status != http.StatusOK {
		t.Fatalf("login ended before 15 minutes: %d", r.status)
	}
	e.clock = e.clock.Add(2 * time.Minute) // 16 min since the last real activity
	if r := e.background(e.owner, "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatalf("idle login still works: %d", r.status)
	}
	if r := e.get(e.owner, "/"); r.status != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Fatalf("idle page view not sent to login: %d %s", r.status, r.header.Get("Location"))
	}
}

func TestBackgroundRequestsDoNotKeepLoginAlive(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.clock = e.clock.Add(6 * time.Minute)
		e.background(e.owner, "/api/shares")
	}
	if r := e.background(e.owner, "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatalf("polling kept an idle login alive: %d", r.status)
	}
}

func TestActivityKeepsLoginAlive(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 6; i++ { // 60 minutes in total, active every 10
		e.clock = e.clock.Add(10 * time.Minute)
		if r := e.get(e.owner, "/api/shares"); r.status != http.StatusOK {
			t.Fatalf("active login ended at step %d: %d", i, r.status)
		}
	}
}

func TestLoginEndsAfterAbsoluteLifetimeEvenWhenActive(t *testing.T) {
	e := newEnv(t)
	for d := time.Duration(0); d < sessionLifetime; d += 10 * time.Minute {
		e.clock = e.clock.Add(10 * time.Minute)
		e.get(e.owner, "/api/shares")
	}
	if r := e.get(e.owner, "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatalf("login outlived its lifetime: %d", r.status)
	}
}

func TestIdleTimeIsConfigurable(t *testing.T) {
	e := newEnv(t)
	c := defaultSettings()
	c.SessionIdle = 60
	if r := e.api("PUT", "/api/settings", c); r.status != http.StatusBadRequest {
		t.Fatalf("1 minute idle accepted: %d", r.status)
	}
	c.SessionIdle = 600
	if r := e.api("PUT", "/api/settings", c); r.status != http.StatusOK {
		t.Fatalf("save 10 minutes: %d %s", r.status, r.body)
	}
	var st struct {
		Idle int64 `json:"idle_seconds"`
		Left int64 `json:"idle_left"`
	}
	e.background(e.owner, "/api/session").json(t, &st)
	if st.Idle != 600 || st.Left < 570 || st.Left > 600 {
		t.Fatalf("status: %+v", st)
	}
	e.clock = e.clock.Add(11 * time.Minute)
	if r := e.background(e.owner, "/api/session"); r.status != http.StatusUnauthorized {
		t.Fatalf("10 minute setting ignored: %d", r.status)
	}
}

func TestAtMostFiveLoginsLeastRecentlyUsedGoes(t *testing.T) {
	e := newEnv(t)
	clients := []*http.Client{}
	for i := 0; i < maxSessions-1; i++ { // the owner plus 4 = 5
		e.clock = e.clock.Add(time.Minute)
		c := e.client()
		e.login(c, testUser, testPass, "")
		clients = append(clients, c)
	}
	e.clock = e.clock.Add(time.Minute)
	e.get(e.owner, "/api/shares") // owner is now the most recently used; clients[0] the least
	if n := len(e.sessions().Sessions); n != maxSessions {
		t.Fatalf("want %d sessions, got %d", maxSessions, n)
	}
	e.clock = e.clock.Add(time.Minute)
	extra := e.client()
	e.login(extra, testUser, testPass, "")

	l := e.sessions()
	if len(l.Sessions) != maxSessions || l.Max != maxSessions {
		t.Fatalf("cap not enforced: %d sessions, max %d", len(l.Sessions), l.Max)
	}
	if r := e.get(clients[0], "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatal("least recently used login survived the 6th login")
	}
	for i, c := range append(clients[1:], extra, e.owner) {
		if r := e.get(c, "/api/shares"); r.status != http.StatusOK {
			t.Fatalf("login %d wrongly ended: %d", i, r.status)
		}
	}
}

func TestEndOneSession(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(time.Minute)
	other := e.client()
	e.login(other, testUser, testPass, "")

	l := e.sessions()
	if len(l.Sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(l.Sessions))
	}
	var mine, theirs sessionJSON
	for _, s := range l.Sessions {
		if s.Current {
			mine = s
		} else {
			theirs = s
		}
	}
	if mine.ID == 0 || theirs.ID == 0 || mine.ID == theirs.ID {
		t.Fatalf("bad ids: %+v", l.Sessions)
	}
	if r := e.do(other, "DELETE", fmt.Sprintf("/api/account/sessions/%d", mine.ID), nil, nil); r.status != http.StatusForbidden {
		t.Fatalf("delete without CSRF token: %d", r.status)
	}
	if r := e.api("DELETE", fmt.Sprintf("/api/account/sessions/%d", theirs.ID), nil); r.status != http.StatusOK {
		t.Fatalf("end other: %d %s", r.status, r.body)
	}
	if r := e.get(other, "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatal("ended session still works")
	}
	if r := e.get(e.owner, "/api/shares"); r.status != http.StatusOK {
		t.Fatal("ending another session logged this one out")
	}
	if r := e.api("DELETE", fmt.Sprintf("/api/account/sessions/%d", theirs.ID), nil); r.status != http.StatusNotFound {
		t.Fatalf("ending twice: %d", r.status)
	}
	if r := e.api("DELETE", "/api/account/sessions/abc", nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad id: %d", r.status)
	}
	r := e.api("DELETE", fmt.Sprintf("/api/account/sessions/%d", mine.ID), nil)
	if r.status != http.StatusOK {
		t.Fatalf("end current: %d", r.status)
	}
	mustContain(t, r.body, `"current":true`)
	if r := e.get(e.owner, "/api/shares"); r.status != http.StatusUnauthorized {
		t.Fatal("current session survived being ended")
	}
}

func TestCleanupPurgesIdleSessions(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(20 * time.Minute)
	e.srv.CleanupOnce()
	l, err := e.st.ListSessions(1, -1) // idleSince -1: every row not past its absolute expiry
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 0 {
		t.Fatalf("idle session row not purged: %d left", len(l))
	}
}

func TestLoginPageExplainsIdleLogout(t *testing.T) {
	e := newEnv(t)
	r := e.get(e.client(), "/login?idle=1")
	mustContain(t, r.body, "logged out")
}
