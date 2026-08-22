package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// public links on, interstitial on, short TTL so clamping is visible
func linksServer(t *testing.T, mut func(*Config)) (*Server, *tsWrap) {
	t.Helper()
	s, ts := testServerWith(t, func(c *Config) {
		c.PublicLinks = true
		c.AnonLinkMaxTTL = 10 * time.Minute
		c.AnonLinkRateN, c.AnonLinkRatePer, c.AnonLinkBurst = 5, time.Hour, 2
		c.AnonLinkDailyCap = 200
		c.AnonLinkInterstitial = true
		if mut != nil {
			mut(c)
		}
	})
	return s, &tsWrap{ts}
}

func TestAnonLinksOffByDefault(t *testing.T) {
	_, ts := testServer(t) // PublicLinks false
	r, _ := do(t, ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://example.com"}`), nil)
	if r.StatusCode != 401 {
		t.Fatalf("anon link with public links off: %d", r.StatusCode)
	}
}

func TestAnonLinkCreateAndRules(t *testing.T) {
	_, ts := linksServer(t, nil)
	post := func(body, ip string) (int, map[string]any) {
		r, b := do(t, ts.Server, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(body), map[string]string{"X-Forwarded-For": ip})
		return r.StatusCode, jsonBody(t, b)
	}
	// paste host never accepts anonymous link writes; anonymous pastes stay as before
	if r, _ := do(t, ts.Server, "POST", "paste.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://example.com"}`), nil); r.StatusCode != 401 {
		t.Fatalf("anon link on paste host: %d", r.StatusCode)
	}
	// a wrong token is still 401 (no silent downgrade)
	if r, _ := do(t, ts.Server, "POST", "go.example", "/api/links", "wrong", "application/json", strings.NewReader(`{"url":"https://example.com"}`), nil); r.StatusCode != 401 {
		t.Fatalf("bad token: %d", r.StatusCode)
	}
	// happy path: random 6-char slug, anon:true, ttl clamped (30d requested → 10m)
	before := time.Now()
	code, m := post(`{"url":"https://example.com/docs?x=1","ttl":"30d"}`, "203.0.113.20")
	if code != 201 || m["anon"] != true {
		t.Fatalf("anon create: %d %v", code, m)
	}
	slug := m["slug"].(string)
	if len(slug) != 6 {
		t.Errorf("anonymous slug should be 6 chars, got %q", slug)
	}
	exp, _ := time.Parse(time.RFC3339Nano, m["expires_at"].(string))
	if exp.IsZero() || exp.After(before.Add(11*time.Minute)) {
		t.Errorf("anon ttl must be clamped to 10m, got %v", exp)
	}
	// forever is clamped too
	if code, m := post(`{"url":"https://example.org","ttl":"0"}`, "203.0.113.21"); code != 201 || m["expires_at"] == nil {
		t.Fatalf("ttl 0 must be clamped: %d %v", code, m)
	}
	// custom slug → 400
	if code, m := post(`{"url":"https://example.com","slug":"mine"}`, "203.0.113.22"); code != 400 || !strings.Contains(m["error"].(string), "token") {
		t.Fatalf("custom slug: %d %v", code, m)
	}
	// destination rules → 400
	// (the limiter counts attempts, like pastes — so each bad URL comes from its own IP)
	for i, bad := range []string{
		`{"url":"http://localhost:8080/x"}`, `{"url":"http://127.0.0.1/"}`, `{"url":"http://10.1.2.3/"}`,
		`{"url":"http://192.168.1.1/"}`, `{"url":"http://169.254.169.254/latest"}`, `{"url":"http://[::1]/"}`,
		`{"url":"https://user:pw@example.com/"}`, `{"url":"ftp://example.com/f"}`, `{"url":"https://go.example/k8s"}`,
		`{"url":"https://example.com/` + strings.Repeat("a", 2100) + `"}`,
	} {
		if code, _ := post(bad, fmt.Sprintf("203.0.113.%d", 30+i)); code != 400 {
			t.Errorf("%s should be 400, got %d", bad[:min(len(bad), 60)], code)
		}
	}
	// a normal public host with a port is fine
	if code, _ := post(`{"url":"https://example.net:8443/ok"}`, "203.0.113.24"); code != 201 {
		t.Errorf("public host with port: %d", code)
	}
	// interstitial: browsers get a 200 page naming the target + /go + report link, no scripts; curl gets 302
	r, b := do(t, ts.Server, "GET", "go.example", "/"+slug, "", "", nil, map[string]string{"Accept": "text/html,*/*"})
	html := string(b)
	if r.StatusCode != 200 || !strings.Contains(html, "https://example.com/docs?x=1") || !strings.Contains(html, "/"+slug+"/go") ||
		!strings.Contains(html, "report") || strings.Contains(html, "<script") {
		t.Fatalf("interstitial: %d script=%v body=%.200s", r.StatusCode, strings.Contains(html, "<script"), html)
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'none'") || r.Header.Get("X-Robots-Tag") == "" ||
		r.Header.Get("Referrer-Policy") != "no-referrer" || r.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("interstitial headers: csp=%q robots=%q ref=%q cc=%q", r.Header.Get("Content-Security-Policy"), r.Header.Get("X-Robots-Tag"), r.Header.Get("Referrer-Policy"), r.Header.Get("Cache-Control"))
	}
	if strings.Contains(html, "203.0.113.20") {
		t.Error("interstitial leaks the creator ip")
	}
	r, _ = do(t, ts.Server, "GET", "go.example", "/"+slug+"/go", "", "", nil, map[string]string{"Accept": "text/html"})
	if r.StatusCode != 302 || r.Header.Get("Location") != "https://example.com/docs?x=1" || r.Header.Get("Referrer-Policy") != "no-referrer" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("/go: %d loc=%q ref=%q cc=%q", r.StatusCode, r.Header.Get("Location"), r.Header.Get("Referrer-Policy"), r.Header.Get("Cache-Control"))
	}
	r, _ = do(t, ts.Server, "GET", "go.example", "/"+slug, "", "", nil, nil) // curl: no text/html
	if r.StatusCode != 302 || r.Header.Get("Location") != "https://example.com/docs?x=1" {
		t.Fatalf("non-browser on anon slug: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// token links are unaffected: custom slug, ttl 0, direct 302 even for browsers, not anon
	r, b = do(t, ts.Server, "POST", "go.example", "/api/links", "secret", "application/json", strings.NewReader(`{"url":"https://example.com/tok","slug":"tok","ttl":"0"}`), nil)
	if r.StatusCode != 201 {
		t.Fatalf("token link: %d %s", r.StatusCode, b)
	}
	if m := jsonBody(t, b); m["anon"] != false || m["expires_at"] != nil || m["slug"] != "tok" {
		t.Errorf("token link shape: %v", m)
	}
	if r, _ = do(t, ts.Server, "GET", "go.example", "/tok", "", "", nil, map[string]string{"Accept": "text/html"}); r.StatusCode != 302 {
		t.Errorf("token link must redirect directly for browsers, got %d", r.StatusCode)
	}
	// list (token) shows anon + creator ip for anonymous, no ip for token links
	_, b = do(t, ts.Server, "GET", "go.example", "/api/links", "secret", "", nil, nil)
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	var sawAnon, sawTok bool
	for _, row := range rows {
		if row["slug"] == slug {
			sawAnon = row["anon"] == true && row["ip"] == "203.0.113.20"
		}
		if row["slug"] == "tok" {
			sawTok = row["anon"] == false && row["ip"] == nil
		}
	}
	if !sawAnon || !sawTok {
		t.Errorf("list must expose anon+ip only for anonymous links: %s", b)
	}
	// purge anonymous links only
	if r, _ := do(t, ts.Server, "DELETE", "go.example", "/api/links?anon=1", "", "", nil, nil); r.StatusCode != 401 {
		t.Fatalf("purge needs the token: %d", r.StatusCode)
	}
	r, b = do(t, ts.Server, "DELETE", "go.example", "/api/links?anon=1", "secret", "", nil, nil)
	if r.StatusCode != 200 || jsonBody(t, b)["deleted"].(float64) < 3 {
		t.Fatalf("purge: %d %s", r.StatusCode, b)
	}
	if r, _ := do(t, ts.Server, "GET", "go.example", "/"+slug, "", "", nil, nil); r.StatusCode != 404 {
		t.Errorf("anon link should be gone: %d", r.StatusCode)
	}
	if r, _ := do(t, ts.Server, "GET", "go.example", "/tok", "", "", nil, nil); r.StatusCode != 302 {
		t.Errorf("token link must survive the purge: %d", r.StatusCode)
	}
	if r, _ := do(t, ts.Server, "DELETE", "go.example", "/api/links", "secret", "", nil, nil); r.StatusCode != 400 {
		t.Errorf("bare DELETE /api/links should be 400, got %d", r.StatusCode)
	}
}

func TestAnonLinkInterstitialOff(t *testing.T) {
	_, ts := linksServer(t, func(c *Config) { c.AnonLinkInterstitial = false })
	r, b := do(t, ts.Server, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://example.com/"}`), nil)
	if r.StatusCode != 201 {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	slug := jsonBody(t, b)["slug"].(string)
	if r, _ := do(t, ts.Server, "GET", "go.example", "/"+slug, "", "", nil, map[string]string{"Accept": "text/html"}); r.StatusCode != 302 {
		t.Fatalf("interstitial off → browsers redirect directly, got %d", r.StatusCode)
	}
}

func TestAnonLinkRateLimitAndDailyCap(t *testing.T) {
	_, ts := linksServer(t, func(c *Config) { c.AnonLinkRateN = 1; c.AnonLinkRatePer = time.Hour; c.AnonLinkBurst = 2 })
	hdr := map[string]string{"X-Forwarded-For": "198.51.100.70"}
	body := `{"url":"https://example.com/"}`
	for i := 0; i < 2; i++ {
		if r, b := do(t, ts.Server, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(body), hdr); r.StatusCode != 201 {
			t.Fatalf("burst %d: %d %s", i, r.StatusCode, b)
		}
	}
	r, b := do(t, ts.Server, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(body), hdr)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" || jsonBody(t, b)["error"] != "rate limited" {
		t.Fatalf("3rd anon link: %d retry-after=%q %s", r.StatusCode, r.Header.Get("Retry-After"), b)
	}
	// the paste limiter is separate: an anonymous paste from the same IP still works
	if r, _ := do(t, ts.Server, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("still ok"), hdr); r.StatusCode != 201 {
		t.Errorf("paste limiter should be independent: %d", r.StatusCode)
	}
	// other IPs and the token path are unaffected
	if r, _ := do(t, ts.Server, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(body), map[string]string{"X-Forwarded-For": "198.51.100.71"}); r.StatusCode != 201 {
		t.Errorf("other ip: %d", r.StatusCode)
	}
	if r, _ := do(t, ts.Server, "POST", "go.example", "/api/links", "secret", "application/json", strings.NewReader(body), hdr); r.StatusCode != 201 {
		t.Errorf("token path limited: %d", r.StatusCode)
	}
	// daily cap
	_, ts2 := linksServer(t, func(c *Config) { c.AnonLinkBurst = 50; c.AnonLinkDailyCap = 3 })
	for i := 0; i < 3; i++ {
		if r, _ := do(t, ts2.Server, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(body), map[string]string{"X-Forwarded-For": "198.51.100.72"}); r.StatusCode != 201 {
			t.Fatalf("cap %d: %d", i, r.StatusCode)
		}
	}
	r, b = do(t, ts2.Server, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(body), map[string]string{"X-Forwarded-For": "198.51.100.73"})
	if r.StatusCode != 429 || jsonBody(t, b)["error"] != "daily cap reached" {
		t.Fatalf("daily cap: %d %s", r.StatusCode, b)
	}
	if r, _ := do(t, ts2.Server, "POST", "go.example", "/api/links", "secret", "application/json", strings.NewReader(body), nil); r.StatusCode != 201 {
		t.Errorf("token path hit the daily cap: %d", r.StatusCode)
	}
}

func TestAnonLinksLandingModes(t *testing.T) {
	_, ts := linksServer(t, nil)
	_, b := do(t, ts.Server, "GET", "go.example", "/", "", "", nil, nil)
	html := string(b)
	for _, want := range []string{`data-public-links="1"`, `id="anon-badge"`, `id="token-details"`, `id="l-anon-note"`, `Acceptable use`, `confirmation page`} {
		if !strings.Contains(html, want) {
			t.Errorf("public links landing missing %q", want)
		}
	}
	if strings.Contains(html, `class="card hidden" id="create-form"`) {
		t.Error("public links landing should render the form visible")
	}
	if !strings.Contains(html, `class="grow hidden" id="l-slug-wrap"`) {
		t.Error("slug field should start hidden for anonymous use")
	}
	// paste host is not advertising anonymous links
	_, b = do(t, ts.Server, "GET", "paste.example", "/", "", "", nil, nil)
	if strings.Contains(string(b), `data-public-links`) {
		t.Error("paste landing must not advertise anonymous links")
	}
	// off: the old unlock-only form
	_, ts2 := testServer(t)
	_, b = do(t, ts2, "GET", "go.example", "/", "", "", nil, nil)
	if strings.Contains(string(b), `data-public-links`) || !strings.Contains(string(b), `class="card hidden" id="create-form"`) {
		t.Error("with public links off the form must stay locked")
	}
}

// tsWrap lets the helpers above read ts.Server uniformly.
type tsWrap struct{ Server *httptest.Server }
