package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS, and a token endpoint that
// turns pre-registered codes into RS256-signed id_tokens.
type fakeIdP struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	clientID string
	mu       sync.Mutex
	codes    map[string]map[string]any // code -> extra claims (sub, email, name)
	tokenHit int
}

func newFakeIdP(t *testing.T, clientID string) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, clientID: clientID, codes: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		u := f.srv.URL
		writeJSON(w, 200, map[string]any{
			"issuer": u, "authorization_endpoint": u + "/authorize", "token_endpoint": u + "/token",
			"jwks_uri": u + "/keys", "end_session_endpoint": u + "/endsession",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		pub := key.PublicKey
		writeJSON(w, 200, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokenHit++
		claims, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code_verifier") == "" || !ok {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		now := time.Now().Unix()
		payload := map[string]any{"iss": f.srv.URL, "aud": clientID, "iat": now, "exp": now + 300}
		for k, v := range claims {
			payload[k] = v
		}
		writeJSON(w, 200, map[string]any{"access_token": "at-" + randomID(8), "token_type": "Bearer", "id_token": f.sign(payload)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) sign(payload map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	p, _ := json.Marshal(payload)
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// code registers a one-shot authorization code for the given identity.
func (f *fakeIdP) code(sub, email, name string) string {
	c := "code-" + randomID(8)
	f.mu.Lock()
	f.codes[c] = map[string]any{"sub": sub, "email": email, "name": name}
	f.mu.Unlock()
	return c
}

// fakeBilling answers entitlements for subjects in plans; everyone else is unknown (404).
func fakeBilling(t *testing.T, token string, plans map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != token {
			w.WriteHeader(401)
			return
		}
		sub := strings.TrimPrefix(r.URL.Path, "/internal/entitlements/")
		if p, ok := plans[sub]; ok {
			writeJSON(w, 200, map[string]any{"plan": p, "valid_until": nil})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type acctEnv struct {
	s   *Server
	ts  *httptest.Server
	idp *fakeIdP
}

func accountsServer(t *testing.T, mut func(*Config)) *acctEnv {
	t.Helper()
	idp := newFakeIdP(t, "hop-client")
	st, err := openStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := Config{Listen: ":0", Token: "secret", LinksHost: "go.example", PasteHost: "paste.example",
		MaxPasteBytes: 2 << 20, DefaultPasteTTL: time.Hour, RepoURL: "https://example/repo", TrustProxy: true,
		OIDCIssuer: idp.srv.URL, OIDCClientID: "hop-client", OIDCClientSecret: "shh",
		OIDCRedirectURLs: []string{"https://go.example/auth/callback", "https://paste.example/auth/callback"},
		SessionSecret:    "test-secret", BillingAccountURL: "https://billing.example/account", Plans: defaultPlans()}
	if mut != nil {
		mut(&cfg)
	}
	s := newServer(cfg, st)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Start(ctx)
	return &acctEnv{s: s, ts: ts, idp: idp}
}

func cookieVal(resp *http.Response, name string) (string, *http.Cookie) {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c.Value, c
		}
	}
	return "", nil
}

// signIn drives /login -> fake IdP -> /auth/callback and returns the session cookie value.
func (e *acctEnv) signIn(t *testing.T, host, sub, email, name string) string {
	t.Helper()
	r, _ := do(t, e.ts, "GET", host, "/login?next=/after", "", "", nil, nil)
	if r.StatusCode != 302 {
		t.Fatalf("/login: %d", r.StatusCode)
	}
	loc, _ := url.Parse(r.Header.Get("Location"))
	oc, _ := cookieVal(r, oauthCookie)
	if oc == "" {
		t.Fatal("no oauth cookie")
	}
	code := e.idp.code(sub, email, name)
	r, b := do(t, e.ts, "GET", host, "/auth/callback?code="+code+"&state="+loc.Query().Get("state"), "", "", nil,
		map[string]string{"Cookie": oauthCookie + "=" + oc})
	if r.StatusCode != 302 {
		t.Fatalf("callback: %d %s", r.StatusCode, b)
	}
	if r.Header.Get("Location") != "/after" {
		t.Fatalf("callback redirect %q", r.Header.Get("Location"))
	}
	sc, _ := cookieVal(r, sessionCookie)
	if sc == "" {
		t.Fatal("no session cookie")
	}
	return sc
}

func sess(c string) map[string]string {
	return map[string]string{"Cookie": sessionCookie + "=" + c, "X-Requested-With": "hop"}
}

func TestAccountsDisabled(t *testing.T) {
	_, ts := testServer(t)
	for _, p := range []string{"/login", "/auth/callback", "/account"} {
		if r, _ := do(t, ts, "GET", "go.example", p, "", "", nil, nil); r.StatusCode != 404 {
			t.Fatalf("%s with accounts off: %d", p, r.StatusCode)
		}
	}
	if r, _ := do(t, ts, "GET", "go.example", "/me", "", "", nil, nil); r.StatusCode != 401 {
		t.Fatalf("/me: %d", r.StatusCode)
	}
	if r, b := do(t, ts, "GET", "go.example", "/me", "secret", "", nil, nil); r.StatusCode != 200 || !jhas(b, "kind", "owner") {
		t.Fatalf("/me owner: %d %s", r.StatusCode, b)
	}
	_, b := do(t, ts, "GET", "go.example", "/", "", "", nil, nil)
	if strings.Contains(string(b), "sitenav-user") || strings.Contains(string(b), `href="/login"`) {
		t.Fatal("landing shows sign-in although accounts are off")
	}
	// a hop_u_ token is meaningless when accounts are off: plain 401, never a crash
	if r, _ := do(t, ts, "GET", "go.example", "/api/links", "hop_u_abc", "", nil, nil); r.StatusCode != 401 {
		t.Fatalf("user token with accounts off: %d", r.StatusCode)
	}
}

func TestLoginRedirectAndCallbackChecks(t *testing.T) {
	e := accountsServer(t, nil)
	r, _ := do(t, e.ts, "GET", "paste.example", "/login", "", "", nil, nil)
	if r.StatusCode != 302 {
		t.Fatalf("/login: %d", r.StatusCode)
	}
	loc, err := url.Parse(r.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), e.idp.srv.URL+"/authorize?") {
		t.Fatalf("authorize location %q", r.Header.Get("Location"))
	}
	q := loc.Query()
	if q.Get("client_id") != "hop-client" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("redirect_uri") != "https://paste.example/auth/callback" ||
		!strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("authorize query %v", q)
	}
	oc, c := cookieVal(r, oauthCookie)
	if oc == "" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("oauth cookie %+v", c)
	}
	// Secure when the proxy says https
	r, _ = do(t, e.ts, "GET", "paste.example", "/login", "", "", nil, map[string]string{"X-Forwarded-Proto": "https"})
	if _, c := cookieVal(r, oauthCookie); c == nil || !c.Secure {
		t.Fatal("oauth cookie not Secure behind https proxy")
	}
	// callback without the cookie, with a wrong state, with an IdP error
	if r, _ := do(t, e.ts, "GET", "paste.example", "/auth/callback?code=x&state="+q.Get("state"), "", "", nil, nil); r.StatusCode != 400 {
		t.Fatalf("callback without cookie: %d", r.StatusCode)
	}
	if r, _ := do(t, e.ts, "GET", "paste.example", "/auth/callback?code=x&state=wrong", "", "", nil, map[string]string{"Cookie": oauthCookie + "=" + oc}); r.StatusCode != 400 {
		t.Fatalf("state mismatch: %d", r.StatusCode)
	}
	if r, b := do(t, e.ts, "GET", "paste.example", "/auth/callback?error=access_denied&state="+q.Get("state"), "", "", nil, map[string]string{"Cookie": oauthCookie + "=" + oc}); r.StatusCode != 400 || !strings.Contains(string(b), "access_denied") {
		t.Fatalf("idp error: %d %s", r.StatusCode, b)
	}
	// a forged cookie is rejected
	if r, _ := do(t, e.ts, "GET", "paste.example", "/auth/callback?code=x&state="+q.Get("state"), "", "", nil, map[string]string{"Cookie": oauthCookie + "=AAAA"}); r.StatusCode != 400 {
		t.Fatalf("forged oauth cookie: %d", r.StatusCode)
	}
	// bad code at the IdP -> 502, no session
	if r, _ := do(t, e.ts, "GET", "paste.example", "/auth/callback?code=nope&state="+q.Get("state"), "", "", nil, map[string]string{"Cookie": oauthCookie + "=" + oc}); r.StatusCode != 502 {
		t.Fatalf("bad code: %d", r.StatusCode)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	e := accountsServer(t, nil)
	sc := e.signIn(t, "go.example", "u-1", "ann@example.com", "Ann")
	r, b := do(t, e.ts, "GET", "go.example", "/me", "", "", nil, sess(sc))
	var me map[string]any
	if r.StatusCode != 200 || json.Unmarshal(b, &me) != nil || me["sub"] != "u-1" || me["email"] != "ann@example.com" || me["plan"] != "free" || me["via"] != "session" {
		t.Fatalf("/me: %d %s", r.StatusCode, b)
	}
	// stored user
	u, err := e.s.st.GetUser(context.Background(), "u-1")
	if err != nil || u.Email != "ann@example.com" || u.Name != "Ann" {
		t.Fatalf("user row: %+v %v", u, err)
	}
	// landing shows the account area + session mode; no token card
	_, b = do(t, e.ts, "GET", "go.example", "/", "", "", nil, sess(sc))
	h := string(b)
	for _, want := range []string{"ann@example.com", `href="/logout"`, `href="/account"`, `data-user="ann@example.com"`, `data-plan="free"`, `id="user-badge"`} {
		if !strings.Contains(h, want) {
			t.Fatalf("landing (signed in) lacks %q", want)
		}
	}
	if strings.Contains(h, `id="token-card"`) || strings.Contains(h, `id="token-details"`) {
		t.Fatal("landing shows the token unlock while signed in")
	}
	// signed-out landing shows Sign in / Sign up
	_, b = do(t, e.ts, "GET", "go.example", "/", "", "", nil, nil)
	if !strings.Contains(string(b), `href="/login"`) || !strings.Contains(string(b), "Sign up") {
		t.Fatal("landing lacks sign-in links")
	}
	// /account
	r, b = do(t, e.ts, "GET", "go.example", "/account", "", "", nil, sess(sc))
	if r.StatusCode != 200 || !strings.Contains(string(b), "ann@example.com") || !strings.Contains(string(b), "https://billing.example/account") || !strings.Contains(string(b), "256.0 KiB") {
		t.Fatalf("/account: %d", r.StatusCode)
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("/account CSP %q", csp)
	}
	if r, _ := do(t, e.ts, "GET", "go.example", "/account", "", "", nil, nil); r.StatusCode != 302 || r.Header.Get("Location") != "/login?next=/account" {
		t.Fatalf("/account anonymous: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// tampered / foreign-key session is ignored (anonymous), not an error
	if r, _ := do(t, e.ts, "GET", "go.example", "/me", "", "", nil, sess(sc[:len(sc)-2]+"zz")); r.StatusCode != 401 {
		t.Fatalf("tampered session: %d", r.StatusCode)
	}
	// logout clears the cookie and goes to the IdP end_session endpoint
	r, _ = do(t, e.ts, "GET", "go.example", "/logout", "", "", nil, sess(sc))
	if r.StatusCode != 302 {
		t.Fatalf("/logout: %d", r.StatusCode)
	}
	if _, c := cookieVal(r, sessionCookie); c == nil || c.MaxAge >= 0 {
		t.Fatalf("logout did not clear the session cookie: %+v", c)
	}
	if l := r.Header.Get("Location"); !strings.HasPrefix(l, e.idp.srv.URL+"/endsession?") || !strings.Contains(l, "post_logout_redirect_uri=") {
		t.Fatalf("logout location %q", l)
	}
}

func TestOwnershipScoping(t *testing.T) {
	e := accountsServer(t, func(c *Config) {
		c.PublicPastes = true
		c.AnonMaxBytes = 1024
		c.AnonMaxTTL = time.Hour
		c.AnonRateN, c.AnonRatePer, c.AnonBurst, c.AnonDailyCap = 10, time.Hour, 10, 100
	})
	a := e.signIn(t, "go.example", "u-a", "a@example.com", "A")
	b := e.signIn(t, "go.example", "u-b", "b@example.com", "B")

	// A: custom slug, direct redirect, a paste
	r, body := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://a.example/x","slug":"mine"}`), sess(a))
	if r.StatusCode != 201 || !jhas(body, "slug", "mine") || !jhas(body, "owned", "true") {
		t.Fatalf("A link: %d %s", r.StatusCode, body)
	}
	if r, _ := do(t, e.ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("a's paste"), sess(a)); r.StatusCode != 201 {
		t.Fatalf("A paste: %d", r.StatusCode)
	}
	if r, _ := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://b.example/y","slug":"bees"}`), sess(b)); r.StatusCode != 201 {
		t.Fatalf("B link: %d", r.StatusCode)
	}
	// an anonymous paste (nobody's)
	if r, _ := do(t, e.ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("anon"), map[string]string{"X-Forwarded-For": "203.0.113.9"}); r.StatusCode != 201 {
		t.Fatalf("anon paste: %d", r.StatusCode)
	}
	// the user's link redirects directly (no interstitial), even for browsers
	r, _ = do(t, e.ts, "GET", "go.example", "/mine", "", "", nil, map[string]string{"Accept": "text/html"})
	if r.StatusCode != 302 || r.Header.Get("Location") != "https://a.example/x" {
		t.Fatalf("user link redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// lists are scoped
	var links []map[string]any
	_, body = do(t, e.ts, "GET", "go.example", "/api/links", "", "", nil, sess(a))
	_ = json.Unmarshal(body, &links)
	if len(links) != 1 || links[0]["slug"] != "mine" {
		t.Fatalf("A sees %s", body)
	}
	if _, ok := links[0]["owner_sub"]; ok {
		t.Fatal("owner_sub leaked to a user")
	}
	var pastes []map[string]any
	_, body = do(t, e.ts, "GET", "paste.example", "/api/pastes", "", "", nil, sess(b))
	_ = json.Unmarshal(body, &pastes)
	if len(pastes) != 0 {
		t.Fatalf("B sees pastes %s", body)
	}
	_, body = do(t, e.ts, "GET", "go.example", "/api/links", "secret", "", nil, nil)
	_ = json.Unmarshal(body, &links)
	if len(links) != 2 || (links[0]["owner_sub"] == nil && links[1]["owner_sub"] == nil) {
		t.Fatalf("owner sees %s", body)
	}
	_, body = do(t, e.ts, "GET", "paste.example", "/api/pastes", "secret", "", nil, nil)
	_ = json.Unmarshal(body, &pastes)
	if len(pastes) != 2 {
		t.Fatalf("owner sees pastes %s", body)
	}
	// deletes are scoped: B cannot delete A's, A can, owner can delete anything
	if r, _ := do(t, e.ts, "DELETE", "go.example", "/api/links/mine", "", "", nil, sess(b)); r.StatusCode != 404 {
		t.Fatalf("B deleting A's link: %d", r.StatusCode)
	}
	if r, _ := do(t, e.ts, "DELETE", "go.example", "/api/links/mine", "", "", nil, sess(a)); r.StatusCode != 204 {
		t.Fatalf("A deleting own link: %d", r.StatusCode)
	}
	if r, _ := do(t, e.ts, "DELETE", "go.example", "/api/links/bees", "secret", "", nil, nil); r.StatusCode != 204 {
		t.Fatalf("owner deleting B's link: %d", r.StatusCode)
	}
	// CSRF: a cookie session without the same-origin marker cannot write
	if r, _ := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://c.example"}`), map[string]string{"Cookie": sessionCookie + "=" + a, "Sec-Fetch-Site": "cross-site"}); r.StatusCode != 403 {
		t.Fatalf("cross-site session write: %d", r.StatusCode)
	}
	// an anonymous link with accounts on is unchanged (none configured here -> 401, not 503)
	if r, _ := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://c.example"}`), nil); r.StatusCode != 401 {
		t.Fatalf("anonymous link with accounts enabled: %d", r.StatusCode)
	}
}

func TestUserTokens(t *testing.T) {
	e := accountsServer(t, nil)
	a := e.signIn(t, "paste.example", "u-a", "a@example.com", "A")
	b := e.signIn(t, "paste.example", "u-b", "b@example.com", "B")
	// owner token cannot mint user tokens; anonymous cannot either
	if r, _ := do(t, e.ts, "POST", "paste.example", "/api/tokens", "secret", "application/json", strings.NewReader(`{}`), nil); r.StatusCode != 403 {
		t.Fatalf("owner POST /api/tokens: %d", r.StatusCode)
	}
	if r, _ := do(t, e.ts, "POST", "paste.example", "/api/tokens", "", "application/json", strings.NewReader(`{}`), nil); r.StatusCode != 401 {
		t.Fatalf("anon POST /api/tokens: %d", r.StatusCode)
	}
	r, body := do(t, e.ts, "POST", "paste.example", "/api/tokens", "", "application/json", strings.NewReader(`{"name":"laptop"}`), sess(a))
	var tok struct{ ID, Name, Token string }
	if r.StatusCode != 201 || json.Unmarshal(body, &tok) != nil || !strings.HasPrefix(tok.Token, userTokenPrefix) || tok.Name != "laptop" {
		t.Fatalf("create token: %d %s", r.StatusCode, body)
	}
	// listed (without the secret), scoped per user
	_, body = do(t, e.ts, "GET", "paste.example", "/api/tokens", "", "", nil, sess(a))
	if !strings.Contains(string(body), tok.ID) || strings.Contains(string(body), tok.Token) {
		t.Fatalf("token list %s", body)
	}
	if _, body := do(t, e.ts, "GET", "paste.example", "/api/tokens", "", "", nil, sess(b)); strings.Contains(string(body), tok.ID) {
		t.Fatalf("B sees A's token: %s", body)
	}
	// the token authenticates the API as A (no CSRF marker needed), scoped, plan-limited
	r, body = do(t, e.ts, "POST", "paste.example", "/api/pastes", tok.Token, "text/plain", strings.NewReader("via token"), nil)
	if r.StatusCode != 201 || !jhas(body, "owned", "true") {
		t.Fatalf("paste via user token: %d %s", r.StatusCode, body)
	}
	if r, _ := do(t, e.ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("b's"), sess(b)); r.StatusCode != 201 {
		t.Fatalf("B paste: %d", r.StatusCode)
	}
	var pastes []map[string]any
	_, body = do(t, e.ts, "GET", "paste.example", "/api/pastes", tok.Token, "", nil, nil)
	_ = json.Unmarshal(body, &pastes)
	if len(pastes) != 1 || pastes[0]["title"] == nil {
		t.Fatalf("hop ls via token sees %s", body)
	}
	r, body = do(t, e.ts, "GET", "paste.example", "/me", tok.Token, "", nil, nil)
	if r.StatusCode != 200 || !jhas(body, "via", "token") || !jhas(body, "sub", "u-a") {
		t.Fatalf("/me via token: %d %s", r.StatusCode, body)
	}
	// the token works on the other host too, and cannot manage tokens itself? (it can: same principal)
	if r, _ := do(t, e.ts, "GET", "go.example", "/api/links", tok.Token, "", nil, nil); r.StatusCode != 200 {
		t.Fatalf("token on links host: %d", r.StatusCode)
	}
	// last_used is touched
	time.Sleep(50 * time.Millisecond)
	ts, _ := e.s.st.ListUserTokens(context.Background(), "u-a")
	if len(ts) != 1 || ts[0].LastUsed == nil {
		t.Fatalf("last_used not recorded: %+v", ts)
	}
	// revoke: B cannot, A can; afterwards the token is rejected outright (401, no downgrade)
	if r, _ := do(t, e.ts, "DELETE", "paste.example", "/api/tokens/"+tok.ID, "", "", nil, sess(b)); r.StatusCode != 404 {
		t.Fatalf("B revoking A's token: %d", r.StatusCode)
	}
	if r, _ := do(t, e.ts, "DELETE", "paste.example", "/api/tokens/"+tok.ID, "", "", nil, sess(a)); r.StatusCode != 204 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if r, _ := do(t, e.ts, "GET", "paste.example", "/api/pastes", tok.Token, "", nil, nil); r.StatusCode != 401 {
		t.Fatalf("revoked token: %d", r.StatusCode)
	}
	if r, _ := do(t, e.ts, "GET", "paste.example", "/api/pastes", userTokenPrefix+"unknown", "", nil, nil); r.StatusCode != 401 {
		t.Fatalf("unknown user token: %d", r.StatusCode)
	}
	// at most 10 tokens
	for i := 0; i < 10; i++ {
		if r, _ := do(t, e.ts, "POST", "paste.example", "/api/tokens", "", "application/json", strings.NewReader(`{}`), sess(a)); r.StatusCode != 201 {
			t.Fatalf("token %d: %d", i, r.StatusCode)
		}
	}
	if r, _ := do(t, e.ts, "POST", "paste.example", "/api/tokens", "", "application/json", strings.NewReader(`{}`), sess(a)); r.StatusCode != 409 {
		t.Fatalf("11th token: %d", r.StatusCode)
	}
}

func TestPlanLimits(t *testing.T) {
	bill := fakeBilling(t, "int-tok", map[string]string{"u-pro": "pro"})
	e := accountsServer(t, func(c *Config) {
		c.BillingURL, c.BillingToken = bill.URL, "int-tok"
		pl := c.Plans["free"]
		pl.MaxItems = 3
		c.Plans["free"] = pl
	})
	free := e.signIn(t, "paste.example", "u-free", "f@example.com", "F")
	pro := e.signIn(t, "paste.example", "u-pro", "p@example.com", "P")

	// plan shows up in /me and on the page
	if _, b := do(t, e.ts, "GET", "paste.example", "/me", "", "", nil, sess(pro)); !jhas(b, "plan", "pro") {
		t.Fatalf("/me pro: %s", b)
	}
	if _, b := do(t, e.ts, "GET", "paste.example", "/", "", "", nil, sess(pro)); !strings.Contains(string(b), `data-plan="pro"`) || !strings.Contains(string(b), `data-plan-forever="1"`) || !strings.Contains(string(b), "1.0 MiB") {
		t.Fatalf("landing pro lacks plan data")
	}
	if _, b := do(t, e.ts, "GET", "paste.example", "/", "", "", nil, sess(free)); !strings.Contains(string(b), `data-plan-forever="0"`) || strings.Contains(string(b), `<option value="0">never</option>`) {
		t.Fatalf("free landing offers never / wrong forever flag")
	}
	big := strings.Repeat("x", 300<<10)
	// free: 256 KiB cap, TTL clamped to 30d even when asked for forever
	if r, b := do(t, e.ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader(big), sess(free)); r.StatusCode != 413 || !strings.Contains(string(b), "free") {
		t.Fatalf("free 300 KiB: %d %s", r.StatusCode, b)
	}
	r, b := do(t, e.ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("keep"), mergeHdr(sess(free), "X-TTL", "0"))
	var out struct {
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if r.StatusCode != 201 || json.Unmarshal(b, &out) != nil || out.ExpiresAt == nil || time.Until(*out.ExpiresAt) > 31*24*time.Hour {
		t.Fatalf("free forever paste: %d %s", r.StatusCode, b)
	}
	r, b = do(t, e.ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("long"), mergeHdr(sess(free), "X-TTL", "365d"))
	if r.StatusCode != 201 || json.Unmarshal(b, &out) != nil || out.ExpiresAt == nil || time.Until(*out.ExpiresAt) > 31*24*time.Hour {
		t.Fatalf("free 365d paste not clamped: %d %s", r.StatusCode, b)
	}
	// free link TTL is clamped too
	r, b = do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://x.example/","ttl":"0"}`), sess(free))
	if r.StatusCode != 201 || json.Unmarshal(b, &out) != nil || out.ExpiresAt == nil {
		t.Fatalf("free forever link: %d %s", r.StatusCode, b)
	}
	// item cap: free has 3 already -> 4th refused; deleting one frees a slot
	if r, b := do(t, e.ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader("four"), sess(free)); r.StatusCode != 429 || !strings.Contains(string(b), "at most 3") {
		t.Fatalf("item cap: %d %s", r.StatusCode, b)
	}
	// pro: 300 KiB fine, forever allowed
	r, b = do(t, e.ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader(big), mergeHdr(sess(pro), "X-TTL", "0"))
	if r.StatusCode != 201 || json.Unmarshal(b, &out) != nil || out.ExpiresAt != nil {
		t.Fatalf("pro forever 300 KiB: %d %.80s", r.StatusCode, b)
	}
	// but not beyond 1 MiB
	if r, _ := do(t, e.ts, "POST", "paste.example", "/api/pastes", "", "text/plain", strings.NewReader(strings.Repeat("x", (1<<20)+1)), sess(pro)); r.StatusCode != 413 {
		t.Fatalf("pro 1 MiB+1: %d", r.StatusCode)
	}
	// owner token: instance limits, untouched by plans
	if r, _ := do(t, e.ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader(strings.Repeat("x", (1<<20)+1)), nil); r.StatusCode != 201 {
		t.Fatalf("owner 1 MiB+1: %d", r.StatusCode)
	}
	// billing is cached: the entitlement call happened once per sub so far
	// billing down -> free (never an error)
	bill.Close()
	e.s.accounts.billing.cache = map[string]planCacheEntry{}
	if _, b := do(t, e.ts, "GET", "paste.example", "/me", "", "", nil, sess(pro)); !jhas(b, "plan", "free") {
		t.Fatalf("/me with billing down: %s", b)
	}
}

func TestPlanRateLimit(t *testing.T) {
	e := accountsServer(t, func(c *Config) {
		pl := c.Plans["free"]
		pl.RatePerHour, pl.Burst = 1, 2
		c.Plans["free"] = pl
	})
	a := e.signIn(t, "go.example", "u-a", "a@example.com", "A")
	b := e.signIn(t, "go.example", "u-b", "b@example.com", "B")
	for i := 0; i < 2; i++ {
		if r, _ := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://x.example/"}`), sess(a)); r.StatusCode != 201 {
			t.Fatalf("link %d: %d", i, r.StatusCode)
		}
	}
	r, _ := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://x.example/"}`), sess(a))
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("3rd link: %d", r.StatusCode)
	}
	// per user, not global
	if r, _ := do(t, e.ts, "POST", "go.example", "/api/links", "", "application/json", strings.NewReader(`{"url":"https://x.example/"}`), sess(b)); r.StatusCode != 201 {
		t.Fatalf("B after A's limit: %d", r.StatusCode)
	}
}

func TestDiscoveryNeverBlocksServing(t *testing.T) {
	// the IdP is down at start: pages serve, /login says 503, a later retry fixes it
	idp := newFakeIdP(t, "hop-client")
	issuer := idp.srv.URL
	idp.srv.Close()
	st, err := openStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := Config{Listen: ":0", Token: "secret", LinksHost: "go.example", PasteHost: "paste.example",
		MaxPasteBytes: 1024, DefaultPasteTTL: time.Hour, RepoURL: "https://example/repo",
		OIDCIssuer: issuer, OIDCClientID: "hop-client", SessionSecret: "x", Plans: defaultPlans()}
	s := newServer(cfg, st)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	s.Start(ctx)
	if time.Since(start) > 5*time.Second {
		t.Fatal("Start blocked on discovery")
	}
	if r, _ := do(t, ts, "GET", "go.example", "/", "", "", nil, nil); r.StatusCode != 200 {
		t.Fatalf("landing with IdP down: %d", r.StatusCode)
	}
	if r, _ := do(t, ts, "GET", "go.example", "/login", "", "", nil, nil); r.StatusCode != 503 {
		t.Fatalf("/login with IdP down: %d", r.StatusCode)
	}
	if r, _ := do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader("ok"), nil); r.StatusCode != 201 {
		t.Fatalf("owner API with IdP down: %d", r.StatusCode)
	}
	if s.accounts.tryInit(ctx) {
		t.Fatal("tryInit succeeded against a closed IdP")
	}
}

func mergeHdr(h map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// jhas reports whether the JSON object in b has key with the given (stringified) value.
func jhas(b []byte, key, val string) bool {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	v, ok := m[key]
	return ok && fmt.Sprint(v) == val
}
