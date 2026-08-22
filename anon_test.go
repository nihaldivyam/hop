package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testServerWith is testServer with a hook to tweak the config (public pastes etc.).
func testServerWith(t *testing.T, mut func(*Config)) (*Server, *httptest.Server) {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := Config{Listen: ":0", Token: "secret", LinksHost: "go.example", PasteHost: "paste.example",
		MaxPasteBytes: 1024, DefaultPasteTTL: time.Hour, RepoURL: "https://example/repo", TrustProxy: true,
		PublicPastes: true, AnonMaxBytes: 100, AnonMaxTTL: 10 * time.Minute,
		AnonRateN: 5, AnonRatePer: time.Hour, AnonBurst: 2, AnonDailyCap: 200}
	if mut != nil {
		mut(&cfg)
	}
	s := newServer(cfg, st)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func jsonBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return m
}

func TestAnonPastesOffByDefault(t *testing.T) {
	_, ts := testServer(t) // PublicPastes false
	r, _ := do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("hello"), nil)
	if r.StatusCode != 401 {
		t.Fatalf("anon create with public pastes off: %d", r.StatusCode)
	}
}

func TestAnonPasteCreateAndLimits(t *testing.T) {
	_, ts := testServerWith(t, nil)
	// links host never accepts anonymous writes
	if r, _ := do(t, ts, "POST", "go.example", "/api/pastes", "", "text/plain", strings.NewReader("x"), nil); r.StatusCode != 401 {
		t.Fatalf("anon on links host: %d", r.StatusCode)
	}
	if r, _ := do(t, ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://e.com"}`), nil); r.StatusCode != 401 {
		t.Fatalf("anon link create: %d", r.StatusCode)
	}
	// a wrong token is still a wrong token (no silent downgrade to anonymous)
	if r, _ := do(t, ts, "POST", "paste.example", "/api/pastes", "wrong", "text/plain", strings.NewReader("x"), nil); r.StatusCode != 401 {
		t.Fatalf("bad token should be 401: %d", r.StatusCode)
	}
	// happy path: no token, paste host, small text, forced short expiry
	before := time.Now()
	r, b := do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("hello anon"),
		map[string]string{"X-Forwarded-For": "203.0.113.9", "X-TTL": "30d"})
	if r.StatusCode != 201 {
		t.Fatalf("anon create: %d %s", r.StatusCode, b)
	}
	m := jsonBody(t, b)
	if m["anon"] != true {
		t.Errorf("response should flag anon: %v", m)
	}
	exp, _ := time.Parse(time.RFC3339Nano, m["expires_at"].(string))
	if exp.IsZero() || exp.After(before.Add(10*time.Minute+time.Minute)) {
		t.Errorf("anon ttl must be clamped to 10m, got expires_at=%v", exp)
	}
	id := m["id"].(string)
	// "forever" is not available anonymously either
	r, b = do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("forever?"), map[string]string{"X-TTL": "0", "X-Forwarded-For": "203.0.113.10"})
	if r.StatusCode != 201 || jsonBody(t, b)["expires_at"] == nil {
		t.Fatalf("anon ttl 0 must be clamped, got %d %s", r.StatusCode, b)
	}
	// size cap: 100 bytes ok, 101 → 413 mentioning the limit
	r, _ = do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader(strings.Repeat("a", 100)), map[string]string{"X-Forwarded-For": "203.0.113.11"})
	if r.StatusCode != 201 {
		t.Fatalf("100 bytes should pass: %d", r.StatusCode)
	}
	r, b = do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader(strings.Repeat("a", 101)), map[string]string{"X-Forwarded-For": "203.0.113.12"})
	if r.StatusCode != 413 || !strings.Contains(string(b), "100 bytes") {
		t.Fatalf("101 bytes: %d %s", r.StatusCode, b)
	}
	// binary refused
	r, _ = do(t, ts, "POST", "paste.example", "/api/pastes", "", "application/octet-stream", strings.NewReader("ab\x00cd"), map[string]string{"X-Forwarded-For": "203.0.113.13"})
	if r.StatusCode != 415 {
		t.Fatalf("binary anon paste: %d", r.StatusCode)
	}
	// token path unaffected: bigger body, ttl 0 allowed, not anon
	r, b = do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader(strings.Repeat("b", 1000)), map[string]string{"X-TTL": "0"})
	if r.StatusCode != 201 {
		t.Fatalf("token paste: %d %s", r.StatusCode, b)
	}
	if m := jsonBody(t, b); m["anon"] != false || m["expires_at"] != nil {
		t.Errorf("token paste should be non-anon and forever: %v", m)
	}
	// list (token) shows anon + creator ip; the public view shows the report line only for anon
	_, b = do(t, ts, "GET", "paste.example", "/api/pastes", "secret", "", nil, nil)
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	var sawAnon, sawToken bool
	for _, row := range rows {
		if row["id"] == id {
			sawAnon = row["anon"] == true && row["ip"] == "203.0.113.9"
		}
		if row["anon"] == false {
			sawToken = row["ip"] == nil
		}
	}
	if !sawAnon || !sawToken {
		t.Errorf("list must expose anon+ip for anonymous and no ip for token pastes: %s", b)
	}
	r, b = do(t, ts, "GET", "paste.example", "/"+id, "", "", nil, map[string]string{"Accept": "text/html"})
	if r.StatusCode != 200 || !strings.Contains(string(b), "report abuse") || strings.Contains(string(b), "203.0.113.9") {
		t.Errorf("anon view: %d report-line=%v ip-leak=%v", r.StatusCode, strings.Contains(string(b), "report abuse"), strings.Contains(string(b), "203.0.113.9"))
	}
	var tokenID string
	for _, row := range rows {
		if row["anon"] == false {
			tokenID = row["id"].(string)
		}
	}
	_, b = do(t, ts, "GET", "paste.example", "/"+tokenID, "", "", nil, map[string]string{"Accept": "text/html"})
	if strings.Contains(string(b), "report abuse") {
		t.Error("token paste view must not carry the anonymous footer")
	}
	// purge: DELETE /api/pastes?anon=1 removes anonymous pastes only
	if r, _ := do(t, ts, "DELETE", "paste.example", "/api/pastes?anon=1", "", "", nil, nil); r.StatusCode != 401 {
		t.Fatalf("purge needs the token: %d", r.StatusCode)
	}
	r, b = do(t, ts, "DELETE", "paste.example", "/api/pastes?anon=1", "secret", "", nil, nil)
	if r.StatusCode != 200 || jsonBody(t, b)["deleted"].(float64) < 3 {
		t.Fatalf("purge: %d %s", r.StatusCode, b)
	}
	if r, _ := do(t, ts, "GET", "paste.example", "/"+id, "", "", nil, nil); r.StatusCode != 404 {
		t.Errorf("anon paste should be gone after purge: %d", r.StatusCode)
	}
	if r, _ := do(t, ts, "GET", "paste.example", "/"+tokenID, "", "", nil, nil); r.StatusCode != 200 {
		t.Errorf("token paste must survive the purge: %d", r.StatusCode)
	}
	if r, _ := do(t, ts, "DELETE", "paste.example", "/api/pastes", "secret", "", nil, nil); r.StatusCode != 400 {
		t.Errorf("bare DELETE /api/pastes should be 400, got %d", r.StatusCode)
	}
}

func TestAnonPasteRateLimitAndDailyCap(t *testing.T) {
	// burst 2, then refill is negligible within the test → third request 429 with Retry-After
	_, ts := testServerWith(t, func(c *Config) { c.AnonRateN = 1; c.AnonRatePer = time.Hour; c.AnonBurst = 2 })
	hdr := map[string]string{"X-Forwarded-For": "198.51.100.7"}
	for i := 0; i < 2; i++ {
		if r, b := do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("ok"), hdr); r.StatusCode != 201 {
			t.Fatalf("burst %d: %d %s", i, r.StatusCode, b)
		}
	}
	r, b := do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("too many"), hdr)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("3rd anon post: %d retry-after=%q %s", r.StatusCode, r.Header.Get("Retry-After"), b)
	}
	if m := jsonBody(t, b); m["retry_after_seconds"] == nil || m["error"] != "rate limited" {
		t.Errorf("429 body: %v", m)
	}
	// another IP is unaffected; the token path ignores the anonymous limiter entirely
	if r, _ := do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("other"), map[string]string{"X-Forwarded-For": "198.51.100.8"}); r.StatusCode != 201 {
		t.Fatalf("other ip: %d", r.StatusCode)
	}
	if r, _ := do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader("tok"), hdr); r.StatusCode != 201 {
		t.Fatalf("token path limited: %d", r.StatusCode)
	}

	// daily cap (global): with a big burst, the 4th anonymous paste of the day is refused
	_, ts2 := testServerWith(t, func(c *Config) { c.AnonBurst = 50; c.AnonDailyCap = 3 })
	for i := 0; i < 3; i++ {
		if r, _ := do(t, ts2, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("cap"), map[string]string{"X-Forwarded-For": "198.51.100.9"}); r.StatusCode != 201 {
			t.Fatalf("cap %d: %d", i, r.StatusCode)
		}
	}
	r, b = do(t, ts2, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("cap"), map[string]string{"X-Forwarded-For": "198.51.100.10"})
	if r.StatusCode != 429 || jsonBody(t, b)["error"] != "daily cap reached" {
		t.Fatalf("daily cap: %d %s", r.StatusCode, b)
	}
	if r, _ := do(t, ts2, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader("tok"), nil); r.StatusCode != 201 {
		t.Fatalf("token path hit the daily cap: %d", r.StatusCode)
	}
}

func TestAnonLandingModes(t *testing.T) {
	// public pastes on: the paste host renders the form open, the badge, the data attributes; links host unchanged
	_, ts := testServerWith(t, nil)
	_, b := do(t, ts, "GET", "paste.example", "/", "", "", nil, nil)
	html := string(b)
	for _, want := range []string{`data-public-pastes="1"`, `id="anon-badge"`, `id="token-details"`, `Acceptable use`, `data-anon-max-bytes="100"`} {
		if !strings.Contains(html, want) {
			t.Errorf("public paste landing missing %q", want)
		}
	}
	if strings.Contains(html, `class="card hidden" id="create-form"`) {
		t.Error("public paste landing should render the form visible")
	}
	_, b = do(t, ts, "GET", "go.example", "/", "", "", nil, nil)
	if strings.Contains(string(b), `data-public-pastes`) || strings.Contains(string(b), `anon-badge`) {
		t.Error("links landing must not advertise anonymous writes")
	}
	// off: the old unlock-only form
	_, ts2 := testServer(t)
	_, b = do(t, ts2, "GET", "paste.example", "/", "", "", nil, nil)
	if strings.Contains(string(b), `data-public-pastes`) || !strings.Contains(string(b), `class="card hidden" id="create-form"`) {
		t.Error("with public pastes off the form must stay locked")
	}
}

func TestParseRate(t *testing.T) {
	n, per, err := parseRate("5/1h")
	if err != nil || n != 5 || per != time.Hour {
		t.Fatalf("5/1h: %d %v %v", n, per, err)
	}
	if _, _, err := parseRate("nope"); err == nil {
		t.Error("bad rate accepted")
	}
	if _, _, err := parseRate("0/1h"); err == nil {
		t.Error("zero rate accepted")
	}
}
