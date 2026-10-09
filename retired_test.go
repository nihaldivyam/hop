package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

const hold = 90 * 24 * time.Hour

// expire makes every link and paste look past its expiry and runs the janitor's sweep.
func expire(t *testing.T, s *Server) {
	t.Helper()
	past := time.Now().Add(-time.Minute).Unix()
	for _, q := range []string{`UPDATE links SET expires_at = ?`, `UPDATE pastes SET expires_at = ?`} {
		if _, err := s.st.db.Exec(q, past); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.st.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A slug or paste id used to be free the moment its item expired or was deleted, so a URL
// someone had shared could start serving another person's link or text. Names are now
// held for a while; only whoever had the name (or the owner token) can take it back.
func TestRetiredNamesAreHeld(t *testing.T) {
	e := accountsServer(t, func(c *Config) { c.NameHold = hold })
	ann := e.signIn(t, "go.example", "u-ann", "ann@example.com", "Ann")
	bob := e.signIn(t, "go.example", "u-bob", "bob@example.com", "Bob")
	link := func(hdr map[string]string, token, slug, target string) int {
		r, _ := do(t, e.ts, "POST", "go.example", "/api/links", token, "application/json", strings.NewReader(`{"url":"`+target+`","slug":"`+slug+`"}`), hdr)
		return r.StatusCode
	}
	paste := func(hdr map[string]string, token, id, text string) int {
		r, _ := do(t, e.ts, "POST", "paste.example", "/api/pastes", token, "text/plain", strings.NewReader(text), mergeHdr(hdr, "X-Id", id))
		return r.StatusCode
	}

	// expiry: Ann's slug and paste name run out
	if link(sess(ann), "", "docs", "https://ann.example/") != 201 || paste(sess(ann), "", "ann-notes", "mine") != 201 {
		t.Fatal("setup failed")
	}
	expire(t, e.s)
	if r, _ := do(t, e.ts, "GET", "go.example", "/docs", "", "", nil, nil); r.StatusCode != 404 {
		t.Fatalf("expired link still served: %d", r.StatusCode)
	}
	if got := link(sess(bob), "", "docs", "https://evil.example/"); got != http.StatusConflict {
		t.Fatalf("Bob claiming Ann's expired slug: %d, want 409", got)
	}
	if got := paste(sess(bob), "", "ann-notes", "not ann's"); got != http.StatusConflict {
		t.Fatalf("Bob claiming Ann's expired paste name: %d, want 409", got)
	}
	// a browser opening the old paste URL is not offered an editor for it
	r, b := do(t, e.ts, "GET", "paste.example", "/ann-notes", "", "", nil, map[string]string{"Accept": "text/html"})
	if r.StatusCode != 404 || strings.Contains(string(b), `data-create-id="ann-notes"`) {
		t.Fatalf("held paste name offered for the taking: %d", r.StatusCode)
	}
	// Ann herself gets both back
	if link(sess(ann), "", "docs", "https://ann.example/v2") != 201 || paste(sess(ann), "", "ann-notes", "mine again") != 201 {
		t.Fatal("the previous holder must be able to take a name back")
	}

	// deletion: same rule (fix a typo by deleting and re-creating; nobody can snipe it)
	if r, _ := do(t, e.ts, "DELETE", "go.example", "/api/links/docs", "", "", nil, sess(ann)); r.StatusCode != 204 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if got := link(sess(bob), "", "docs", "https://evil.example/"); got != http.StatusConflict {
		t.Fatalf("Bob claiming Ann's deleted slug: %d, want 409", got)
	}
	if got := link(sess(ann), "", "docs", "https://ann.example/v3"); got != 201 {
		t.Fatalf("Ann re-creating her deleted slug: %d", got)
	}

	// the owner token can release anything; its own retired names are held against users
	if link(nil, "secret", "official", "https://example.com/") != 201 {
		t.Fatal("owner create")
	}
	if r, _ := do(t, e.ts, "DELETE", "go.example", "/api/links/official", "secret", "", nil, nil); r.StatusCode != 204 {
		t.Fatalf("owner delete: %d", r.StatusCode)
	}
	if got := link(sess(bob), "", "official", "https://evil.example/"); got != http.StatusConflict {
		t.Fatalf("a user claiming the owner's retired slug: %d, want 409", got)
	}
	if got := link(nil, "secret", "official", "https://example.com/again"); got != 201 {
		t.Fatalf("owner re-creating: %d", got)
	}
	if r, _ := do(t, e.ts, "DELETE", "go.example", "/api/links/docs", "", "", nil, sess(ann)); r.StatusCode != 204 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if got := link(nil, "secret", "docs", "https://example.com/taken-over"); got != 201 {
		t.Fatalf("owner token taking a held slug: %d", got)
	}

	// the hold runs out
	if paste(sess(ann), "", "old-name", "x") != 201 {
		t.Fatal("setup")
	}
	if r, _ := do(t, e.ts, "DELETE", "paste.example", "/api/pastes/old-name", "", "", nil, sess(ann)); r.StatusCode != 204 {
		t.Fatalf("delete paste: %d", r.StatusCode)
	}
	if _, err := e.s.st.db.Exec(`UPDATE retired SET retired_at = ? WHERE id = 'old-name'`, time.Now().Add(-hold-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if held, _ := e.s.st.NameHeld(context.Background(), "paste", "old-name"); held {
		t.Fatal("a hold past its time still counts")
	}
	if got := paste(sess(bob), "", "old-name", "bob's now"); got != 201 {
		t.Fatalf("after the hold a name is free again: %d", got)
	}
	// and the janitor clears holds that ran out
	if paste(sess(ann), "", "gone", "x") != 201 {
		t.Fatal("setup")
	}
	do(t, e.ts, "DELETE", "paste.example", "/api/pastes/gone", "", "", nil, sess(ann))
	e.s.st.db.Exec(`UPDATE retired SET retired_at = ? WHERE id = 'gone'`, time.Now().Add(-hold-time.Hour).Unix())
	e.s.st.Sweep(context.Background())
	var n int
	e.s.st.db.QueryRow(`SELECT COUNT(*) FROM retired WHERE id = 'gone'`).Scan(&n)
	if n != 0 {
		t.Fatal("sweep left a hold that had run out")
	}
}

func TestAnonymousNamesAreHeldToo(t *testing.T) {
	s, ts := linksServer(t, func(c *Config) {
		c.NameHold = hold
		c.PublicPastes, c.AnonMaxBytes, c.AnonMaxTTL = true, 32<<10, 24*time.Hour
		c.AnonRateN, c.AnonRatePer, c.AnonBurst, c.AnonDailyCap = 1000, time.Hour, 1000, -1
	})
	anon := func(id string) int {
		r, _ := do(t, ts.Server, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("hello"), map[string]string{"X-Id": id})
		return r.StatusCode
	}
	if got := anon("meeting-notes"); got != 201 {
		t.Fatalf("anonymous named paste: %d", got)
	}
	expire(t, s)
	if got := anon("meeting-notes"); got != http.StatusConflict {
		t.Fatalf("another anonymous visitor re-claiming the name: %d, want 409", got)
	}
	// the create page tells people so
	r, b := do(t, ts.Server, "GET", "paste.example", "/a-free-name", "", "", nil, map[string]string{"Accept": "text/html"})
	if r.StatusCode != 200 || !strings.Contains(string(b), "stays reserved for 90 days") {
		t.Fatalf("create page should explain the hold: %d", r.StatusCode)
	}
	// the kill switch retires names as well
	if got := anon("spam-1"); got != 201 {
		t.Fatalf("setup: %d", got)
	}
	if r, _ := do(t, ts.Server, "DELETE", "paste.example", "/api/pastes?anon=1", "secret", "", nil, nil); r.StatusCode != 200 {
		t.Fatalf("purge: %d", r.StatusCode)
	}
	if got := anon("spam-1"); got != http.StatusConflict {
		t.Fatalf("purged name re-claimed at once: %d, want 409", got)
	}
}

func TestNameHoldOffKeepsOldBehaviour(t *testing.T) {
	e := accountsServer(t, nil) // NameHold 0
	ann := e.signIn(t, "go.example", "u-ann", "ann@example.com", "Ann")
	bob := e.signIn(t, "go.example", "u-bob", "bob@example.com", "Bob")
	mk := func(c string) int {
		r, _ := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://x.example/","slug":"s"}`), sess(c))
		return r.StatusCode
	}
	if mk(ann) != 201 {
		t.Fatal("setup")
	}
	expire(t, e.s)
	if got := mk(bob); got != 201 {
		t.Fatalf("with no hold a name is free at once: %d", got)
	}
}

// The limiter forgot any bucket idle for five minutes, and a forgotten key starts again
// with a full burst. At the anonymous rate (5 an hour, burst 2) that is 2 creates every few
// minutes instead of one every 12.
func TestLimiterCleanupDoesNotRefill(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	l := newLimiter(5.0/3600, 2)
	l.now = func() time.Time { return now }
	l.lastGC = now
	if !l.allow("ip") || !l.allow("ip") || l.allow("ip") {
		t.Fatal("burst should be exactly 2")
	}
	// 6 idle minutes: the cleanup runs (it is due after 5), half a token has dripped in
	now = now.Add(6 * time.Minute)
	l.allow("someone-else") // any call triggers the cleanup
	if l.allow("ip") {
		t.Fatal("6 minutes after spending the burst a new create was allowed: the cleanup refilled the bucket")
	}
	// the real refill still works: 12 minutes per token
	now = now.Add(7 * time.Minute)
	if !l.allow("ip") {
		t.Fatal("a token should have arrived after 13 minutes")
	}
	if l.allow("ip") {
		t.Fatal("only one token should have arrived")
	}
	// and fully refilled buckets are forgotten, so the map does not grow without bound
	now = now.Add(2 * time.Hour)
	l.allow("trigger")
	now = now.Add(6 * time.Minute)
	l.allow("trigger-2")
	l.mu.Lock()
	_, kept := l.b["ip"]
	l.mu.Unlock()
	if kept {
		t.Fatal("a bucket that has been full for hours should have been dropped")
	}
	if !l.allow("ip") || !l.allow("ip") || l.allow("ip") {
		t.Fatal("after a real full refill the burst is back, and no more")
	}
}

func TestNoDeadSiteLinks(t *testing.T) {
	for _, name := range []string{"landing.html", "paste.html", "interstitial.html", "create.html", "account.html", "_sitenav.html"} {
		b, err := assets.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		// the one-page site with #about / #contact anchors is gone
		if strings.Contains(string(b), "divyam.top/#") {
			t.Errorf("%s still links to an anchor of the retired one-page site", name)
		}
	}
}
