package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// After sign-in the browser is sent to ?next=. Browsers read a backslash as a slash and
// drop tabs and newlines inside URLs, so "/\evil.com" and "/<tab>/evil.com" both leave the
// site although they start with a single slash.
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/account":              "/account",
		"/":                     "/",
		"/x?y=1&z=/a#frag":      "/x?y=1&z=/a#frag",
		"/%5Cevil.com":          "/%5Cevil.com", // still encoded: a path on this site, not a backslash
		"":                      "/account",
		"account":               "/account",
		"//evil.com":            "/account",
		"///evil.com":           "/account",
		"/\\evil.com":           "/account",
		"/\\/evil.com":          "/account",
		"/\t/evil.com":          "/account",
		"/\n/evil.com":          "/account",
		"/a\\b":                 "/account",
		"/\x00":                 "/account",
		"/\x7f":                 "/account",
		"https://evil.com":      "/account",
		"javascript:alert(1)":   "/account",
		"/ /evil.com":           "/ /evil.com", // not a URL separator; stays a path here
		"/ok/../still/local":    "/ok/../still/local",
		"/%zz":                  "/account", // not parseable
		"/x\r\nSet-Cookie: a=b": "/account",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginNextCannotLeaveTheSite(t *testing.T) {
	e := accountsServer(t, nil)
	for _, next := range []string{"/%5Cevil.com", "/%09/evil.com", "//evil.com", "https://evil.com/"} {
		r, _ := do(t, e.ts, "GET", "go.example", "/login?next="+next, "", "", nil, nil)
		if r.StatusCode != 302 {
			t.Fatalf("/login: %d", r.StatusCode)
		}
		loc, _ := url.Parse(r.Header.Get("Location"))
		oc, _ := cookieVal(r, oauthCookie)
		r, b := do(t, e.ts, "GET", "go.example", "/auth/callback?code="+e.idp.code("u1", "u1@example.com", "U")+"&state="+loc.Query().Get("state"), "", "", nil,
			map[string]string{"Cookie": oauthCookie + "=" + oc})
		if r.StatusCode != 302 || r.Header.Get("Location") != "/account" {
			t.Fatalf("next=%s: landed on %q (%d %s), want /account", next, r.Header.Get("Location"), r.StatusCode, b)
		}
	}
}

// mutableBilling is a billing fake whose answers can change mid-test.
type mutableBilling struct {
	mu    sync.Mutex
	plans map[string]string
	srv   *httptest.Server
}

func newMutableBilling(t *testing.T, token string, plans map[string]string) *mutableBilling {
	t.Helper()
	m := &mutableBilling{plans: plans}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != token {
			w.WriteHeader(401)
			return
		}
		m.mu.Lock()
		p, ok := m.plans[strings.TrimPrefix(r.URL.Path, "/internal/entitlements/")]
		m.mu.Unlock()
		if !ok {
			p = "free"
		}
		writeJSON(w, 200, map[string]any{"plan": p})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mutableBilling) set(sub, plan string) {
	m.mu.Lock()
	m.plans[sub] = plan
	m.mu.Unlock()
}

// Sign-up is open, so a free account is one form away for anyone. Its links used to
// redirect straight to any destination under a slug of the creator's choosing: a ready-made
// phishing link on this domain. Free-plan links now behave like anonymous ones for a
// browser (a page naming the destination first) and obey the same destination rules.
func TestFreePlanLinksShowConfirmationPage(t *testing.T) {
	bill := newMutableBilling(t, "tok", map[string]string{"u-pro": "pro"})
	e := accountsServer(t, func(c *Config) {
		c.BillingURL, c.BillingToken = bill.srv.URL, "tok"
		c.FreeLinkInterstitial = true
	})
	free := e.signIn(t, "go.example", "u-free", "f@example.com", "F")
	pro := e.signIn(t, "go.example", "u-pro", "p@example.com", "P")
	browser := map[string]string{"Accept": "text/html,application/xhtml+xml"}
	mk := func(hdr map[string]string, token, body string) (*http.Response, []byte) {
		return do(t, e.ts, "POST", "go.example", "/api/links", token, "application/json", strings.NewReader(body), hdr)
	}

	r, b := mk(sess(free), "", `{"url":"https://bank.example/login","slug":"bank-login"}`)
	if r.StatusCode != 201 || !jhas(b, "confirm", "true") {
		t.Fatalf("free link: %d %s", r.StatusCode, b)
	}
	// a browser sees where it leads and has to click through
	r, b = do(t, e.ts, "GET", "go.example", "/bank-login", "", "", nil, browser)
	if r.StatusCode != 200 || !strings.Contains(string(b), "bank.example") || !strings.Contains(string(b), `href="/bank-login/go"`) {
		t.Fatalf("free link in a browser: want the confirmation page, got %d Location=%q", r.StatusCode, r.Header.Get("Location"))
	}
	if strings.Contains(string(b), "created anonymously") || !strings.Contains(string(b), "free account") {
		t.Fatalf("confirmation page should say who made the link: %.300s", b)
	}
	if r, _ := do(t, e.ts, "GET", "go.example", "/bank-login/go", "", "", nil, browser); r.StatusCode != 302 || r.Header.Get("Location") != "https://bank.example/login" {
		t.Fatalf("continue: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// scripts and CLIs are not the audience of the page
	if r, _ := do(t, e.ts, "GET", "go.example", "/bank-login", "", "", nil, nil); r.StatusCode != 302 {
		t.Fatalf("free link for curl: %d", r.StatusCode)
	}

	// paying customers and the owner token keep the direct redirect
	if r, b := mk(sess(pro), "", `{"url":"https://pro.example/","slug":"pro1"}`); r.StatusCode != 201 || !jhas(b, "confirm", "false") {
		t.Fatalf("pro link: %d %s", r.StatusCode, b)
	}
	if r, b := mk(nil, "secret", `{"url":"https://own.example/","slug":"own1"}`); r.StatusCode != 201 || !jhas(b, "confirm", "false") {
		t.Fatalf("owner link: %d %s", r.StatusCode, b)
	}
	for _, slug := range []string{"pro1", "own1"} {
		if r, _ := do(t, e.ts, "GET", "go.example", "/"+slug, "", "", nil, browser); r.StatusCode != 302 {
			t.Fatalf("%s in a browser: want a direct 302, got %d", slug, r.StatusCode)
		}
	}

	// the page follows the owner's plan, not the day the link was made
	bill.set("u-free", "pro")
	e.s.accounts.billing.mu.Lock()
	e.s.accounts.billing.cache = map[string]planCacheEntry{}
	e.s.accounts.billing.mu.Unlock()
	if r, _ := do(t, e.ts, "GET", "go.example", "/bank-login", "", "", nil, browser); r.StatusCode != 302 {
		t.Fatalf("after upgrading, old links should go direct: %d", r.StatusCode)
	}
	bill.set("u-free", "free")
	bill.set("u-pro", "free")
	e.s.accounts.billing.mu.Lock()
	e.s.accounts.billing.cache = map[string]planCacheEntry{}
	e.s.accounts.billing.mu.Unlock()
	if r, _ := do(t, e.ts, "GET", "go.example", "/pro1", "", "", nil, browser); r.StatusCode != 200 {
		t.Fatalf("after Pro lapses, links get the page again: %d", r.StatusCode)
	}

	// destinations: the rules anonymous links already had
	for _, target := range []string{"http://localhost:8080/admin", "http://10.0.0.5/", "http://169.254.169.254/latest/meta-data/",
		"https://user:pw@example.com/", "https://go.example/own1", "https://paste.example/x", "http://[::1]/"} {
		if r, b := mk(sess(free), "", `{"url":"`+target+`"}`); r.StatusCode != 400 {
			t.Fatalf("free link to %s: want 400, got %d %s", target, r.StatusCode, b)
		} else if strings.Contains(string(b), "anonymous") {
			t.Fatalf("message to a signed-in user talks about anonymous links: %s", b)
		}
	}
	// the owner token is not second-guessed (it may well point at something internal)
	if r, b := mk(nil, "secret", `{"url":"http://10.0.0.5/"}`); r.StatusCode != 201 {
		t.Fatalf("owner link to a private address: %d %s", r.StatusCode, b)
	}
}

func TestFreeLinkInterstitialCanBeTurnedOff(t *testing.T) {
	e := accountsServer(t, nil) // FreeLinkInterstitial false, no billing: everyone is free
	free := e.signIn(t, "go.example", "u-free", "f@example.com", "F")
	if r, b := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://x.example/","slug":"x"}`), sess(free)); r.StatusCode != 201 || !jhas(b, "confirm", "false") {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	if r, _ := do(t, e.ts, "GET", "go.example", "/x", "", "", nil, map[string]string{"Accept": "text/html"}); r.StatusCode != 302 {
		t.Fatalf("with the page off a free link redirects directly: %d", r.StatusCode)
	}
}
