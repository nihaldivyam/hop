package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := Config{Listen: ":0", Token: "secret", LinksHost: "go.example", PasteHost: "paste.example",
		MaxPasteBytes: 1024, DefaultPasteTTL: time.Hour, RepoURL: "https://example/repo", TrustProxy: true}
	s := newServer(cfg, st)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, ts *httptest.Server, method, host, path, token, ctype string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, body)
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func TestSlugGeneration(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		s := randomID(5)
		if len(s) != 5 {
			t.Fatalf("len %d", len(s))
		}
		for _, c := range s {
			if !strings.ContainsRune(base58, c) {
				t.Fatalf("bad char %q", c)
			}
		}
		seen[s] = true
	}
	if len(seen) < 495 {
		t.Fatalf("too many collisions: %d unique", len(seen))
	}
	for _, bad := range []string{"api", "healthz", "static", "a b", "", strings.Repeat("x", 65), "../x"} {
		if validSlug(bad) {
			t.Errorf("slug %q should be invalid", bad)
		}
	}
	if !validSlug("k8s_notes-1") {
		t.Error("valid slug rejected")
	}
}

func TestTokenAuth(t *testing.T) {
	_, ts := testServer(t)
	body := `{"url":"https://example.com/x"}`
	if r, _ := do(t, ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(body), nil); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r, _ := do(t, ts, "POST", "go.example", "/api/links", "wrong", "application/json", strings.NewReader(body), nil); r.StatusCode != 401 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	if r, _ := do(t, ts, "POST", "go.example", "/api/links", "secret", "application/json", strings.NewReader(body), nil); r.StatusCode != 201 {
		t.Fatalf("good token: %d", r.StatusCode)
	}
}

func TestLinksRedirectAndHostRouting(t *testing.T) {
	_, ts := testServer(t)
	r, b := do(t, ts, "POST", "go.example", "/api/links", "secret", "application/json",
		strings.NewReader(`{"url":"https://example.com/docs","slug":"docs"}`), nil)
	if r.StatusCode != 201 {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	var out map[string]any
	json.Unmarshal(b, &out)
	if out["short_url"] != "https://go.example/docs" {
		t.Fatalf("short_url %v", out["short_url"])
	}
	r, _ = do(t, ts, "GET", "go.example", "/docs", "", "", nil, nil)
	if r.StatusCode != 302 || r.Header.Get("Location") != "https://example.com/docs" {
		t.Fatalf("redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// the same path on the paste host is a paste lookup, not a link
	if r, _ = do(t, ts, "GET", "paste.example", "/docs", "", "", nil, nil); r.StatusCode != 404 {
		t.Fatalf("paste host should 404 for a link slug: %d", r.StatusCode)
	}
	// hits are counted (async) — poll briefly
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, b = do(t, ts, "GET", "go.example", "/api/links", "secret", "", nil, nil)
		if strings.Contains(string(b), `"hits": 1`) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(b), `"hits": 1`) {
		t.Fatalf("hits not counted: %s", b)
	}
	// duplicate slug / bad url / reserved slug
	if r, _ = do(t, ts, "POST", "go.example", "/api/links", "secret", "application/json", strings.NewReader(`{"url":"https://e.com","slug":"docs"}`), nil); r.StatusCode != 409 {
		t.Fatalf("dup: %d", r.StatusCode)
	}
	if r, _ = do(t, ts, "POST", "go.example", "/api/links", "secret", "application/json", strings.NewReader(`{"url":"javascript:alert(1)"}`), nil); r.StatusCode != 400 {
		t.Fatalf("bad url: %d", r.StatusCode)
	}
	if r, _ = do(t, ts, "POST", "go.example", "/api/links", "secret", "application/json", strings.NewReader(`{"url":"https://e.com","slug":"api"}`), nil); r.StatusCode != 400 {
		t.Fatalf("reserved: %d", r.StatusCode)
	}
	if r, _ = do(t, ts, "DELETE", "go.example", "/api/links/docs", "secret", "", nil, nil); r.StatusCode != 204 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if r, _ = do(t, ts, "GET", "go.example", "/docs", "", "", nil, nil); r.StatusCode != 404 {
		t.Fatalf("after delete: %d", r.StatusCode)
	}
}

func TestExpiryAndJanitor(t *testing.T) {
	s, ts := testServer(t)
	r, _ := do(t, ts, "POST", "go.example", "/api/links", "secret", "application/json",
		strings.NewReader(`{"url":"https://example.com","slug":"tmp","ttl":"1ms"}`), nil)
	if r.StatusCode != 201 {
		t.Fatalf("create: %d", r.StatusCode)
	}
	time.Sleep(1100 * time.Millisecond) // expiry granularity is one second
	if r, _ = do(t, ts, "GET", "go.example", "/tmp", "", "", nil, nil); r.StatusCode != 404 {
		t.Fatalf("expired link served: %d", r.StatusCode)
	}
	n, err := s.st.Sweep(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	if _, err := parseTTL("7d"); err != nil {
		t.Fatal(err)
	}
	if d, _ := parseTTL("2w"); d != 14*24*time.Hour {
		t.Fatalf("2w = %v", d)
	}
	if _, err := parseTTL("-1h"); err == nil {
		t.Fatal("negative ttl accepted")
	}
}

func TestPastesEscapingAndNegotiation(t *testing.T) {
	_, ts := testServer(t)
	content := "<script>alert('x')</script>\nsecond line"
	r, b := do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader(content),
		map[string]string{"X-Title": "evil.html", "X-Lang": "html"})
	if r.StatusCode != 201 {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	var out map[string]any
	json.Unmarshal(b, &out)
	id := out["id"].(string)
	if out["url"] != "https://paste.example/"+id {
		t.Fatalf("url %v", out["url"])
	}
	// default: text/plain, verbatim
	r, b = do(t, ts, "GET", "paste.example", "/"+id, "", "", nil, nil)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") || string(b) != content {
		t.Fatalf("plain: %d %q %q", r.StatusCode, r.Header.Get("Content-Type"), b)
	}
	// browser: HTML, escaped, CSP, numbered
	r, b = do(t, ts, "GET", "paste.example", "/"+id, "", "", nil, map[string]string{"Accept": "text/html"})
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("html ct: %q", r.Header.Get("Content-Type"))
	}
	if strings.Contains(string(b), "<script>alert") || !strings.Contains(string(b), "&lt;script&gt;") {
		t.Fatalf("not escaped: %s", b)
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("csp: %q", r.Header.Get("Content-Security-Policy"))
	}
	if strings.Count(string(b), "<li>") != 2 {
		t.Fatalf("line count: %s", b)
	}
	// extension form and ?html=1 also give HTML; /raw never does
	if r, _ = do(t, ts, "GET", "paste.example", "/"+id+".go", "", "", nil, nil); !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
		t.Fatal("ext should give html")
	}
	if r, _ = do(t, ts, "GET", "paste.example", "/"+id+"/raw", "", "", nil, map[string]string{"Accept": "text/html"}); !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
		t.Fatal("raw should be plain")
	}
	// too large
	r, _ = do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader(strings.Repeat("a", 2048)), nil)
	if r.StatusCode != 413 {
		t.Fatalf("too large: %d", r.StatusCode)
	}
	// list has no content; delete works
	_, b = do(t, ts, "GET", "paste.example", "/api/pastes", "secret", "", nil, nil)
	if strings.Contains(string(b), "alert") {
		t.Fatal("list leaked content")
	}
	if r, _ = do(t, ts, "DELETE", "paste.example", "/api/pastes/"+id, "secret", "", nil, nil); r.StatusCode != 204 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if r, _ = do(t, ts, "GET", "paste.example", "/"+id, "", "", nil, nil); r.StatusCode != 404 {
		t.Fatalf("after delete: %d", r.StatusCode)
	}
}

func TestWritesDisabledWithoutToken(t *testing.T) {
	st, _ := openStore(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	ts := httptest.NewServer(newServer(Config{LinksHost: "go.example", PasteHost: "paste.example", MaxPasteBytes: 100}, st))
	defer ts.Close()
	r, _ := do(t, ts, "POST", "go.example", "/api/links", "anything", "application/json", strings.NewReader(`{"url":"https://e.com"}`), nil)
	if r.StatusCode != 503 {
		t.Fatalf("expected 503, got %d", r.StatusCode)
	}
	if r, _ = do(t, ts, "GET", "go.example", "/healthz", "", "", nil, nil); r.StatusCode != 200 {
		t.Fatalf("healthz: %d", r.StatusCode)
	}
}
