package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The links host was renamed once (go.divyam.top -> short.divyam.top) and the old name
// stays in front of hop: its pages redirect to the new one, its /api keeps answering.
// HOP_LINKS_ALIASES is how hop learns about such former names.

func TestLinksAliasesConfig(t *testing.T) {
	for _, k := range []string{"HOP_LINKS_HOST", "HOP_PASTE_HOST", "HOP_LINKS_ALIASES"} {
		t.Setenv(k, "")
	}
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.LinksHost != defaultLinksHost || len(c.LinksAliases) != 0 {
		t.Fatalf("defaults: links=%q aliases=%v", c.LinksHost, c.LinksAliases)
	}
	// the CLI's built-in API URL has to be the server's built-in links host
	if defaultAPI != "https://"+defaultLinksHost {
		t.Fatalf("CLI default %q does not point at the default links host %q", defaultAPI, defaultLinksHost)
	}

	t.Setenv("HOP_LINKS_HOST", "Short.Example")
	t.Setenv("HOP_PASTE_HOST", "paste.example")
	t.Setenv("HOP_LINKS_ALIASES", " Go.Example , ,old.example,short.example, go.example ")
	c, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// lowercased, trimmed, empties and duplicates dropped, the links host itself is not its own alias
	if want := []string{"go.example", "old.example"}; !reflect.DeepEqual(c.LinksAliases, want) {
		t.Fatalf("aliases = %v, want %v", c.LinksAliases, want)
	}

	// the paste host cannot double as a links alias: routing sends it to pastes
	t.Setenv("HOP_LINKS_ALIASES", "go.example,PASTE.example")
	if _, err = loadConfig(); err == nil || !strings.Contains(err.Error(), "HOP_LINKS_ALIASES") {
		t.Fatalf("an alias equal to the paste host must be refused, got %v", err)
	}
}

// The old name redirects to the new one, so a link that points at it would bounce
// short -> old -> short. Unvetted links (anonymous, free plan) may not point there, with
// or without the trailing dot that makes a hostname look different.
func TestFormerLinksHostIsNotADestination(t *testing.T) {
	_, ts := linksServer(t, func(c *Config) { c.LinksAliases = []string{"old.example"} })
	n := 0
	post := func(target string) (int, string) {
		n++ // one client address per request: the anonymous limiter counts refused attempts too
		r, b := do(t, ts.Server, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"`+target+`"}`), map[string]string{"X-Forwarded-For": fmt.Sprintf("203.0.113.%d", n)})
		return r.StatusCode, string(b)
	}
	for _, target := range []string{
		"https://old.example/abc", "https://OLD.example/abc", "https://old.example./abc",
		"https://go.example./abc", "https://paste.example./abc", "http://localhost./x",
	} {
		if code, body := post(target); code != 400 {
			t.Errorf("anonymous link to %s: %d %s, want 400", target, code, body)
		}
	}
	if code, body := post("https://elsewhere.example/abc"); code != 201 {
		t.Fatalf("an ordinary destination must still work: %d %s", code, body)
	}

	// same rule for a free-plan account, and the owner token is not second-guessed
	e := accountsServer(t, func(c *Config) { c.LinksAliases = []string{"old.example"} })
	free := e.signIn(t, "go.example", "u-free", "f@example.com", "F")
	if r, b := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://old.example./abc"}`), sess(free)); r.StatusCode != 400 || strings.Contains(string(b), "anonymous") {
		t.Fatalf("free-plan link to the former host: %d %s", r.StatusCode, b)
	}
	if r, b := do(t, e.ts, "POST", "go.example", "/api/links", "secret", "application/json", strings.NewReader(`{"url":"https://old.example/abc"}`), nil); r.StatusCode != 201 {
		t.Fatalf("owner token link to the former host: %d %s", r.StatusCode, b)
	}
}

// API clients configured with the old name (the CLI, scripts) keep working through it,
// and what they get back names the current host.
func TestFormerLinksHostStillServesTheAPI(t *testing.T) {
	_, ts := linksServer(t, func(c *Config) { c.LinksAliases = []string{"old.example"} })
	r, b := do(t, ts.Server, "POST", "old.example", "/api/links", "secret", "application/json", strings.NewReader(`{"url":"https://example.com/","slug":"viaold"}`), nil)
	if m := jsonBody(t, b); r.StatusCode != 201 || m["short_url"] != "https://go.example/viaold" || m["anon"] != false {
		t.Fatalf("token create on the former host: %d %s", r.StatusCode, b)
	}
	if r, _ := do(t, ts.Server, "GET", "old.example", "/viaold", "", "", nil, nil); r.StatusCode != 302 || r.Header.Get("Location") != "https://example.com/" {
		t.Fatalf("slug on the former host: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// a page left open on the old name can still create an anonymous link …
	r, b = do(t, ts.Server, "POST", "old.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://example.com/a"}`), map[string]string{"X-Forwarded-For": "203.0.113.8"})
	if m := jsonBody(t, b); r.StatusCode != 201 || m["anon"] != true || !strings.HasPrefix(m["short_url"].(string), "https://go.example/") {
		t.Fatalf("anonymous create on the former host: %d %s", r.StatusCode, b)
	}
	// … but a hostname hop was never told about gets no anonymous writes
	if r, _ := do(t, ts.Server, "POST", "stranger.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://example.com/b"}`), map[string]string{"X-Forwarded-For": "203.0.113.9"}); r.StatusCode != 401 {
		t.Fatalf("anonymous create on an unknown host: %d, want 401", r.StatusCode)
	}
}

// The landing page's description used to spell out "go." + "divyam.top".
func TestLandingDescriptionNamesTheConfiguredHosts(t *testing.T) {
	_, ts := testServer(t)
	for host, want := range map[string]string{"go.example": "go.example", "paste.example": "paste.example"} {
		_, b := do(t, ts, "GET", host, "/", "", "", nil, nil)
		html := string(b)
		i := strings.Index(html, `<meta name="description"`)
		if i < 0 {
			t.Fatalf("%s: no meta description", host)
		}
		meta := html[i : i+strings.Index(html[i:], ">")]
		if !strings.Contains(meta, want) || strings.Contains(meta, "divyam.top") {
			t.Errorf("%s: description should name %s and no hard-coded host: %s", host, want, meta)
		}
	}
}
