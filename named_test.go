package main

import (
	"strings"
	"testing"
	"time"
)

// "Name your own paste URL": X-Id on the API, the create page for free names,
// and the rules around it (1–15 chars, reserved names, taken names).
func TestCustomPasteIDsAPI(t *testing.T) {
	_, ts := testServer(t) // token only
	post := func(id, token string, hdr map[string]string) (int, map[string]any, string) {
		h := map[string]string{}
		for k, v := range hdr {
			h[k] = v
		}
		if id != "" {
			h["X-Id"] = id
		}
		r, b := do(t, ts, "POST", "paste.example", "/api/pastes", token, "text/plain", strings.NewReader("named body"), h)
		if r.StatusCode == 201 {
			return r.StatusCode, jsonBody(t, b), string(b)
		}
		return r.StatusCode, nil, string(b)
	}
	// valid names: 1 char, 15 chars, letters/digits/-/_
	for _, id := range []string{"a", "A1", "my-notes_123", strings.Repeat("x", 15), "9lives"} {
		code, m, b := post(id, "secret", nil)
		if code != 201 || m["id"] != id || !strings.HasSuffix(m["url"].(string), "/"+id) {
			t.Fatalf("custom id %q: %d %s", id, code, b)
		}
	}
	// taken → 409
	if code, _, b := post("my-notes_123", "secret", nil); code != 409 || !strings.Contains(b, "id taken") {
		t.Fatalf("taken id: %d %s", code, b)
	}
	// invalid → 400 with the rule
	for _, id := range []string{strings.Repeat("x", 16), "-starts-dash", "_u", "has.dot", "has space", "api", "raw", "static", "healthz", "go", "admin", "Api"} {
		if code, _, b := post(id, "secret", nil); code != 400 || !strings.Contains(b, "invalid id") {
			t.Fatalf("invalid id %q should be 400: %d %s", id, code, b)
		}
	}
	// ?id= query works too
	r, b := do(t, ts, "POST", "paste.example", "/api/pastes?id=via-query", "secret", "text/plain", strings.NewReader("q"), nil)
	if r.StatusCode != 201 || jsonBody(t, b)["id"] != "via-query" {
		t.Fatalf("?id= : %d %s", r.StatusCode, b)
	}
	// the named paste is served at its URL
	r, b = do(t, ts, "GET", "paste.example", "/my-notes_123", "", "", nil, nil)
	if r.StatusCode != 200 || string(b) != "named body" {
		t.Fatalf("named paste read: %d %q", r.StatusCode, b)
	}
	// no custom id → random 8-char id as before
	code, m, body := post("", "secret", nil)
	if code != 201 || len(m["id"].(string)) != 8 {
		t.Fatalf("random id: %d %s", code, body)
	}
}

func TestCustomPasteIDsAnonymous(t *testing.T) {
	_, ts := testServerWith(t, nil) // public pastes, anon cap 100 B / 10 min
	r, b := do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("anon named"),
		map[string]string{"X-Id": "anon-name", "X-TTL": "30d", "X-Forwarded-For": "203.0.113.50"})
	if r.StatusCode != 201 {
		t.Fatalf("anon custom id: %d %s", r.StatusCode, b)
	}
	m := jsonBody(t, b)
	if m["id"] != "anon-name" || m["anon"] != true {
		t.Fatalf("anon custom id response: %s", b)
	}
	exp, _ := time.Parse(time.RFC3339Nano, m["expires_at"].(string))
	if exp.IsZero() || exp.After(time.Now().Add(11*time.Minute)) {
		t.Errorf("anon named paste must keep the anon TTL clamp: %v", exp)
	}
	// anon limits still apply with a custom id
	r, b = do(t, ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader(strings.Repeat("a", 101)),
		map[string]string{"X-Id": "too-big", "X-Forwarded-For": "203.0.113.51"})
	if r.StatusCode != 413 {
		t.Fatalf("anon size cap with custom id: %d %s", r.StatusCode, b)
	}
	// taken by the anonymous paste → 409 even for the token holder
	r, b = do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader("x"), map[string]string{"X-Id": "anon-name"})
	if r.StatusCode != 409 {
		t.Fatalf("taken (by anon) id: %d %s", r.StatusCode, b)
	}
}

func TestCreatePageForFreeNames(t *testing.T) {
	s, ts := testServerWith(t, nil)
	html := map[string]string{"Accept": "text/html"}
	// browser on a free, valid name → 200 editor for that name
	r, b := do(t, ts, "GET", "paste.example", "/my-notes-123", "", "", nil, html)
	if r.StatusCode != 200 {
		t.Fatalf("create page: %d", r.StatusCode)
	}
	page := string(b)
	for _, want := range []string{`data-create-id="my-notes-123"`, `paste.example/my-notes-123`, `id="p-content"`, `id="p-id"`, `value="my-notes-123"`, `readonly`, `class="sitenav"`, `/static/app.js?v=` + s.assetVer, `X-Id: my-notes-123`} {
		if !strings.Contains(page, want) {
			t.Errorf("create page missing %q", want)
		}
	}
	csp := r.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "connect-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("create page CSP: %s", csp)
	}
	if r.Header.Get("X-Robots-Tag") != "noindex, nofollow" || r.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("create page headers: robots=%q cc=%q", r.Header.Get("X-Robots-Tag"), r.Header.Get("Cache-Control"))
	}
	// anonymous mode badge present when public pastes are on
	if !strings.Contains(page, `id="anon-badge"`) {
		t.Error("create page should show the anonymous badge in public mode")
	}
	// invalid names → 404 page with the rule (browser) …
	for _, bad := range []string{"/" + strings.Repeat("x", 16), "/-dash", "/api"} {
		r, b = do(t, ts, "GET", "paste.example", bad, "", "", nil, html)
		if r.StatusCode != 404 || !strings.Contains(string(b), "1-15 chars") {
			t.Fatalf("invalid name %s: %d (hint missing?)", bad, r.StatusCode)
		}
		if strings.Contains(string(b), "data-create-id") {
			t.Errorf("invalid name %s must not offer the editor", bad)
		}
	}
	// … and a name with an extension is just a 404, not an editor
	r, _ = do(t, ts, "GET", "paste.example", "/free-name.go", "", "", nil, html)
	if r.StatusCode != 404 {
		t.Fatalf("free name with ext: %d", r.StatusCode)
	}
	// non-browser clients get the plain 404
	r, b = do(t, ts, "GET", "paste.example", "/my-notes-123", "", "", nil, nil)
	if r.StatusCode != 404 || strings.Contains(string(b), "<html") {
		t.Fatalf("curl on free name: %d %q", r.StatusCode, b)
	}
	// once created, the same URL is the paste view (no editor)
	r, _ = do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader("package main"), map[string]string{"X-Id": "my-notes-123", "X-Lang": "go"})
	if r.StatusCode != 201 {
		t.Fatalf("create named: %d", r.StatusCode)
	}
	r, b = do(t, ts, "GET", "paste.example", "/my-notes-123", "", "", nil, html)
	if r.StatusCode != 200 || strings.Contains(string(b), "data-create-id") || !strings.Contains(string(b), "package") {
		t.Fatalf("view after create: %d", r.StatusCode)
	}
	if strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src") {
		t.Errorf("paste view must keep the no-script CSP")
	}
	// the landing page offers the custom URL field
	_, b = do(t, ts, "GET", "paste.example", "/", "", "", nil, nil)
	if !strings.Contains(string(b), `id="p-id"`) || !strings.Contains(string(b), `maxlength="15"`) {
		t.Error("landing should render the custom URL field")
	}
	// links host is unaffected: a free slug is a plain 404 for browsers too
	r, _ = do(t, ts, "GET", "go.example", "/free-slug", "", "", nil, html)
	if r.StatusCode != 404 {
		t.Fatalf("links host free slug: %d", r.StatusCode)
	}
}

func TestCreatePageTokenOnlyMode(t *testing.T) {
	_, ts := testServer(t) // public pastes off: the editor still appears but needs an unlock
	r, b := do(t, ts, "GET", "paste.example", "/private-note", "", "", nil, map[string]string{"Accept": "text/html"})
	if r.StatusCode != 200 || !strings.Contains(string(b), `data-create-id="private-note"`) || !strings.Contains(string(b), `id="token"`) {
		t.Fatalf("create page (token mode): %d", r.StatusCode)
	}
	if strings.Contains(string(b), `id="anon-badge"`) {
		t.Error("no anonymous badge when public pastes are off")
	}
}

func TestCLIPasteCustomID(t *testing.T) {
	_, ts := testServer(t)
	cliEnv(t, ts.URL)
	code, out, errOut := runCapture(t, "named via cli\n", "paste", "--id", "cli-name")
	if code != 0 || strings.TrimSpace(out) != "https://paste.example/cli-name" {
		t.Fatalf("paste --id: %d %q %q", code, out, errOut)
	}
	if code, _, errOut = runCapture(t, "again\n", "paste", "--id", "cli-name"); code != 1 || !strings.Contains(errOut, "id taken") {
		t.Fatalf("paste --id taken: %d %q", code, errOut)
	}
	if code, _, errOut = runCapture(t, "x\n", "paste", "--id", strings.Repeat("z", 16)); code != 2 || !strings.Contains(errOut, "1-15") {
		t.Fatalf("paste --id invalid: %d %q", code, errOut)
	}
	// --name is an alias
	if code, out, _ = runCapture(t, "x\n", "paste", "--name", "alias-1"); code != 0 || strings.TrimSpace(out) != "https://paste.example/alias-1" {
		t.Fatalf("paste --name: %d %q", code, out)
	}
}
