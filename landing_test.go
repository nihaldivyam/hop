package main

import (
	"strings"
	"testing"
)

// The landing pages are the only HTML with scripts; they must carry the UI,
// a script-allowing (but 'self'-only) CSP, and hashed, revalidating assets.
func TestLandingPagesAndAssets(t *testing.T) {
	s, ts := testServer(t)
	for _, host := range []string{"go.example", "paste.example"} {
		r, b := do(t, ts, "GET", host, "/", "", "", nil, nil)
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s landing: %d %q", host, r.StatusCode, r.Header.Get("Content-Type"))
		}
		html := string(b)
		for _, want := range []string{`id="token"`, `id="unlock"`, `id="create-form"`, `class="sitenav"`, `/static/app.js?v=` + s.assetVer, `/static/style.css?v=` + s.assetVer} {
			if !strings.Contains(html, want) {
				t.Errorf("%s landing missing %q", host, want)
			}
		}
		csp := r.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s landing CSP missing %q: %s", host, want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s landing CSP allows inline: %s", host, csp)
		}
	}
	// host-specific forms
	_, b := do(t, ts, "GET", "go.example", "/", "", "", nil, nil)
	if !strings.Contains(string(b), `id="l-url"`) || strings.Contains(string(b), `id="p-content"`) {
		t.Error("links host should render the link form only")
	}
	_, b = do(t, ts, "GET", "paste.example", "/", "", "", nil, nil)
	if !strings.Contains(string(b), `id="p-content"`) || strings.Contains(string(b), `id="l-url"`) {
		t.Error("paste host should render the paste form only")
	}
	// assets: types, hashed caching, revalidation
	r, b := do(t, ts, "GET", "go.example", "/static/app.js?v="+s.assetVer, "", "", nil, nil)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(string(b), "hop.token") {
		t.Fatalf("app.js: %d %q", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("versioned asset should be immutable: %q", r.Header.Get("Cache-Control"))
	}
	r, _ = do(t, ts, "GET", "paste.example", "/static/style.css", "", "", nil, nil)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/css") || r.Header.Get("Cache-Control") != "no-cache" || r.Header.Get("ETag") == "" {
		t.Fatalf("style.css: %d %q cc=%q etag=%q", r.StatusCode, r.Header.Get("Content-Type"), r.Header.Get("Cache-Control"), r.Header.Get("ETag"))
	}
	if r, _ = do(t, ts, "GET", "paste.example", "/static/style.css", "", "", nil, map[string]string{"If-None-Match": `"` + s.assetVer + `"`}); r.StatusCode != 304 {
		t.Fatalf("etag revalidation: %d", r.StatusCode)
	}
	if r, _ = do(t, ts, "GET", "go.example", "/static/nope.txt", "", "", nil, nil); r.StatusCode != 404 {
		t.Fatalf("unknown asset: %d", r.StatusCode)
	}
	// the paste view keeps the no-script policy
	r, _ = do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader("x"), nil)
	if r.StatusCode != 201 {
		t.Fatalf("paste: %d", r.StatusCode)
	}
	_, b = do(t, ts, "GET", "paste.example", "/api/pastes", "secret", "", nil, nil)
	id := strings.Split(strings.Split(string(b), `"id": "`)[1], `"`)[0]
	r, _ = do(t, ts, "GET", "paste.example", "/"+id, "", "", nil, map[string]string{"Accept": "text/html"})
	if strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src") {
		t.Errorf("paste view CSP must not allow scripts: %s", r.Header.Get("Content-Security-Policy"))
	}
}
