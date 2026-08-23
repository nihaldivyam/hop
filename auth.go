package main

// auth.go — user accounts: sign in through an OIDC provider (our Zitadel),
// encrypted session cookies, per-user API tokens, plan limits from the billing
// service. Everything here is inert when OIDC_CLIENT_ID is empty: hop then
// behaves exactly as before (owner token + anonymous modes).

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// --- plans -----------------------------------------------------------------------

// planLimits is what a signed-in user may do. The owner token is unlimited and
// anonymous users have the HOP_ANON_* caps; these apply in between.
type planLimits struct {
	Name            string        `json:"name"`
	MaxPasteBytes   int64         `json:"max_paste_bytes"`
	MaxTTL          time.Duration `json:"-"` // 0 = unlimited ("never" allowed)
	DefaultPasteTTL time.Duration `json:"-"`
	RatePerHour     int           `json:"rate_per_hour"`
	Burst           int           `json:"burst"`
	MaxItems        int64         `json:"max_items"`
}

func defaultPlans() map[string]planLimits {
	return map[string]planLimits{
		"free": {Name: "free", MaxPasteBytes: 256 << 10, MaxTTL: 30 * 24 * time.Hour, DefaultPasteTTL: 30 * 24 * time.Hour, RatePerHour: 30, Burst: 10, MaxItems: 500},
		"pro":  {Name: "pro", MaxPasteBytes: 1 << 20, MaxTTL: 0, DefaultPasteTTL: 30 * 24 * time.Hour, RatePerHour: 300, Burst: 30, MaxItems: 10000},
	}
}

// --- principal --------------------------------------------------------------------

type principal struct {
	Kind    string // "owner" (HOP_TOKEN), "user" (signed in / user token), "anon"
	Sub     string
	Email   string
	Name    string
	Plan    string // users only: "free" | "pro"
	Via     string // "session" | "token"
	TokenID string // user token id when Via == "token"
	Admin   bool   // session carries a role listed in HOP_ADMIN_ROLES
}

func (p principal) isOwner() bool { return p.Kind == "owner" }
func (p principal) isUser() bool  { return p.Kind == "user" }
func (p principal) authed() bool  { return p.Kind == "owner" || p.Kind == "user" }
func (p principal) isAdmin() bool { return p.Kind == "user" && p.Admin }

// canSeeAll covers the two callers allowed to browse and delete other people's
// rows, and to see the metadata (creator IP, owner subject) attached to them.
func (p principal) canSeeAll() bool { return p.isOwner() || p.isAdmin() }

// ownerScope is the owner STAMPED ON ROWS THIS PRINCIPAL CREATES: a signed-in
// user's own subject, empty for the owner token. Deliberately not admin-aware —
// an admin's own pastes stay theirs rather than becoming ownerless.
func (p principal) ownerScope() string {
	if p.isUser() {
		return p.Sub
	}
	return ""
}

// viewScope is the store filter for READING AND DELETING. Same as ownerScope for
// ordinary users; empty (= everything) for the owner token and for admins.
func (p principal) viewScope() string {
	if p.isUser() && !p.Admin {
		return p.Sub
	}
	return ""
}

// rolesFrom pulls a flat list of role names out of an id_token claim. Accepts an
// array of strings or a single string; anything else (notably an object, which is
// what Zitadel asserts natively) yields nothing rather than a misleading guess.
func rolesFrom(claims map[string]any, key string) []string {
	switch v := claims[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v != "" {
			return []string{v}
		}
	}
	return nil
}

// hasAdminRole reports whether any of the session's roles is configured as admin.
// Empty cfg.AdminRoles means the feature is off, whatever the IdP claims.
func (s *Server) hasAdminRole(roles []string) bool {
	for _, want := range s.cfg.AdminRoles {
		for _, got := range roles {
			if got == want {
				return true
			}
		}
	}
	return false
}

type ctxKey int

const principalKey ctxKey = 1

func principalFrom(ctx context.Context) principal {
	if p, ok := ctx.Value(principalKey).(principal); ok {
		return p
	}
	return principal{Kind: "anon"}
}

var errBadBearer = errors.New("missing or invalid bearer token")

// --- accounts service ------------------------------------------------------------------

type accounts struct {
	cfg Config
	st  *Store

	mu         sync.RWMutex
	provider   *oidc.Provider
	verifier   *oidc.IDTokenVerifier
	endSession string

	sealKey []byte
	billing *billingClient
	limits  map[string]*limiter // per plan name, keyed by sub inside
}

func newAccounts(cfg Config, st *Store) *accounts {
	secret := cfg.SessionSecret
	if secret == "" {
		b := make([]byte, 32)
		rand.Read(b)
		secret = hex.EncodeToString(b)
		if cfg.OIDCClientID != "" {
			log.Printf("accounts: HOP_SESSION_SECRET is empty — using a random key; sessions will not survive a restart")
		}
	}
	k := sha256.Sum256([]byte(secret))
	a := &accounts{cfg: cfg, st: st, sealKey: k[:], limits: map[string]*limiter{}}
	a.billing = newBillingClient(cfg.BillingURL, cfg.BillingToken)
	for name, pl := range cfg.Plans {
		a.limits[name] = newLimiter(float64(pl.RatePerHour)/3600, float64(max(pl.Burst, 1)))
	}
	return a
}

func (a *accounts) enabled() bool { return a.cfg.OIDCClientID != "" && a.cfg.OIDCIssuer != "" }

// start performs OIDC discovery: once synchronously (fast path when the IdP is
// up), then keeps retrying in the background so hop never refuses to serve
// links and pastes just because the IdP is still booting.
func (a *accounts) start(ctx context.Context) {
	if !a.enabled() {
		return
	}
	if a.tryInit(ctx) {
		return
	}
	go func() {
		delay := 5 * time.Second
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if a.tryInit(ctx) {
				return
			}
			if delay < time.Minute {
				delay *= 2
			}
		}
	}()
}

func (a *accounts) tryInit(ctx context.Context) bool {
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(c, a.cfg.OIDCIssuer)
	if err != nil {
		log.Printf("accounts: OIDC discovery for %s failed: %v (will retry)", a.cfg.OIDCIssuer, err)
		return false
	}
	var extra struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = p.Claims(&extra)
	a.mu.Lock()
	a.provider = p
	a.verifier = p.Verifier(&oidc.Config{ClientID: a.cfg.OIDCClientID})
	a.endSession = extra.EndSession
	a.mu.Unlock()
	log.Printf("accounts: sign-in ready via %s", a.cfg.OIDCIssuer)
	return true
}

func (a *accounts) ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.provider != nil
}

// --- sealed cookies -------------------------------------------------------------------

func (a *accounts) seal(v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(a.sealKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, nil)), nil
}

func (a *accounts) open(s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(a.sealKey)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if len(raw) < gcm.NonceSize() {
		return errors.New("short cookie")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, v)
}

const (
	sessionCookie = "hop_session"
	oauthCookie   = "hop_oauth"
	sessionTTL    = 7 * 24 * time.Hour
)

type session struct {
	Sub   string   `json:"sub"`
	Email string   `json:"email"`
	Name  string   `json:"name"`
	Roles []string `json:"roles,omitempty"` // from the id_token; re-read at each sign-in
	Exp   int64    `json:"exp"`
}

type oauthState struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Redirect string `json:"redirect"`
	Next     string `json:"next"`
	Exp      int64  `json:"exp"`
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *accounts) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: secureRequest(r), MaxAge: maxAge}
	if a.cfg.CookieDomain != "" {
		c.Domain = a.cfg.CookieDomain
	}
	http.SetCookie(w, c)
}

func (a *accounts) clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	a.setCookie(w, r, name, "", -1)
}

// sessionFrom reads and validates the session cookie; nil when absent/expired/forged.
func (a *accounts) sessionFrom(r *http.Request) *session {
	if !a.enabled() {
		return nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	var s session
	if err := a.open(c.Value, &s); err != nil || s.Sub == "" || time.Now().Unix() > s.Exp {
		return nil
	}
	return &s
}

// --- identification ----------------------------------------------------------------------

// identify resolves the caller: owner token, user token, session, or anonymous.
// An Authorization header that matches nothing is an error (never a silent downgrade).
func (s *Server) identify(r *http.Request) (principal, error) {
	if auth := r.Header.Get("Authorization"); auth != "" {
		tok := strings.TrimPrefix(auth, "Bearer ")
		if tok == auth || tok == "" {
			return principal{Kind: "anon"}, errBadBearer
		}
		if s.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.Token)) == 1 {
			return principal{Kind: "owner", Via: "token"}, nil
		}
		if strings.HasPrefix(tok, userTokenPrefix) && s.accounts.enabled() {
			sub, id, err := s.st.LookupUserToken(r.Context(), hashToken(tok))
			if err == nil {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_ = s.st.TouchUserToken(ctx, id)
				}()
				u, _ := s.st.GetUser(r.Context(), sub)
				p := principal{Kind: "user", Sub: sub, Via: "token", TokenID: id}
				if u != nil {
					p.Email, p.Name = u.Email, u.Name
				}
				p.Plan = s.accounts.planFor(r.Context(), sub)
				return p, nil
			}
		}
		return principal{Kind: "anon"}, errBadBearer
	}
	if sess := s.accounts.sessionFrom(r); sess != nil {
		p := principal{Kind: "user", Sub: sess.Sub, Email: sess.Email, Name: sess.Name, Via: "session"}
		// Admin comes from the session cookie, which is sealed and re-minted on every
		// sign-in — so revoking the role in the IdP takes effect at the next sign-in,
		// not instantly. Session TTL bounds that window.
		p.Admin = s.hasAdminRole(sess.Roles)
		p.Plan = s.accounts.planFor(r.Context(), sess.Sub)
		return p, nil
	}
	return principal{Kind: "anon"}, nil
}

// sameOrigin is the CSRF gate for cookie-authenticated state changes: the
// landing UI always sends X-Requested-With; browsers add Sec-Fetch-Site.
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("X-Requested-With") == "hop" {
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	}
	return false
}

// --- per-user tokens -----------------------------------------------------------------------

const userTokenPrefix = "hop_u_"

func newUserToken() string { return userTokenPrefix + randomID(32) }

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// --- billing client (plan entitlements) ------------------------------------------------

type billingClient struct {
	url, token string
	http       *http.Client
	mu         sync.Mutex
	cache      map[string]planCacheEntry
}

type planCacheEntry struct {
	plan  string
	until time.Time
}

func newBillingClient(url, token string) *billingClient {
	return &billingClient{url: strings.TrimRight(url, "/"), token: token,
		http: &http.Client{Timeout: 3 * time.Second}, cache: map[string]planCacheEntry{}}
}

// plan returns "free" or "pro" for a subject, cached for 5 minutes; on any
// error it assumes "free" (cached briefly so a down billing service is not hammered).
func (b *billingClient) plan(ctx context.Context, sub string) string {
	if b.url == "" {
		return "free"
	}
	b.mu.Lock()
	if e, ok := b.cache[sub]; ok && time.Now().Before(e.until) {
		b.mu.Unlock()
		return e.plan
	}
	b.mu.Unlock()
	plan, ttl := "free", 30*time.Second
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url+"/internal/entitlements/"+url.PathEscape(sub), nil)
	if err == nil {
		req.Header.Set("X-Internal-Token", b.token)
		resp, err := b.http.Do(req)
		if err == nil {
			var out struct {
				Plan string `json:"plan"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&out) == nil && (out.Plan == "free" || out.Plan == "pro") {
				plan, ttl = out.Plan, 5*time.Minute
			}
			resp.Body.Close()
		}
	}
	b.mu.Lock()
	b.cache[sub] = planCacheEntry{plan: plan, until: time.Now().Add(ttl)}
	b.mu.Unlock()
	return plan
}

func (a *accounts) planFor(ctx context.Context, sub string) string {
	plan := a.billing.plan(ctx, sub)
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.st.SetUserPlanCache(c, sub, plan)
	}()
	return plan
}

func (a *accounts) limitsFor(plan string) planLimits {
	if pl, ok := a.cfg.Plans[plan]; ok {
		return pl
	}
	return a.cfg.Plans["free"]
}

// --- OIDC handlers -----------------------------------------------------------------------

func (s *Server) redirectURIFor(r *http.Request) string {
	host := hostOf(r)
	for _, u := range s.cfg.OIDCRedirectURLs {
		if pu, err := url.Parse(u); err == nil && strings.EqualFold(pu.Hostname(), host) {
			return u
		}
	}
	if len(s.cfg.OIDCRedirectURLs) > 0 {
		return s.cfg.OIDCRedirectURLs[0]
	}
	scheme := "https"
	if !secureRequest(r) && (strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.")) {
		scheme = "http"
	}
	return scheme + "://" + r.Host + "/auth/callback"
}

func (s *Server) oauthConfig(r *http.Request) *oauth2.Config {
	s.accounts.mu.RLock()
	p := s.accounts.provider
	s.accounts.mu.RUnlock()
	return &oauth2.Config{
		ClientID: s.cfg.OIDCClientID, ClientSecret: s.cfg.OIDCClientSecret,
		Endpoint: p.Endpoint(), RedirectURL: s.redirectURIFor(r),
		Scopes: []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

// safeNext only allows same-site relative paths as post-login targets.
func safeNext(n string) string {
	if n == "" || !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.ContainsAny(n, "\r\n") {
		return "/account"
	}
	return n
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.accounts.enabled() {
		http.NotFound(w, r)
		return
	}
	if !s.accounts.ready() {
		http.Error(w, "sign-in is not ready yet (identity provider unreachable) — try again in a minute", http.StatusServiceUnavailable)
		return
	}
	st := oauthState{State: randomID(24), Verifier: oauth2.GenerateVerifier(),
		Redirect: s.redirectURIFor(r), Next: safeNext(r.URL.Query().Get("next")), Exp: time.Now().Add(10 * time.Minute).Unix()}
	sealed, err := s.accounts.seal(st)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.accounts.setCookie(w, r, oauthCookie, sealed, 600)
	conf := s.oauthConfig(r)
	conf.RedirectURL = st.Redirect
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, conf.AuthCodeURL(st.State, oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	if !s.accounts.enabled() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	c, err := r.Cookie(oauthCookie)
	if err != nil {
		http.Error(w, "sign-in session expired — start again at /login", http.StatusBadRequest)
		return
	}
	var st oauthState
	if err := s.accounts.open(c.Value, &st); err != nil || time.Now().Unix() > st.Exp {
		http.Error(w, "sign-in session expired — start again at /login", http.StatusBadRequest)
		return
	}
	s.accounts.clearCookie(w, r, oauthCookie)
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "sign-in failed: "+e+" "+r.URL.Query().Get("error_description"), http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("state") == "" || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(st.State)) != 1 {
		http.Error(w, "sign-in failed: state mismatch", http.StatusBadRequest)
		return
	}
	if !s.accounts.ready() {
		http.Error(w, "identity provider unreachable", http.StatusServiceUnavailable)
		return
	}
	conf := s.oauthConfig(r)
	conf.RedirectURL = st.Redirect
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := conf.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		log.Printf("accounts: code exchange failed: %v", err)
		http.Error(w, "sign-in failed: could not exchange the code", http.StatusBadGateway)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		http.Error(w, "sign-in failed: no id_token", http.StatusBadGateway)
		return
	}
	s.accounts.mu.RLock()
	verifier := s.accounts.verifier
	s.accounts.mu.RUnlock()
	idt, err := verifier.Verify(ctx, rawID)
	if err != nil {
		log.Printf("accounts: id_token rejected: %v", err)
		http.Error(w, "sign-in failed: invalid id_token", http.StatusBadGateway)
		return
	}
	var claims struct {
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	_ = idt.Claims(&claims)
	var rawClaims map[string]any
	_ = idt.Claims(&rawClaims)
	roles := rolesFrom(rawClaims, s.cfg.RolesClaim)
	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}
	if err := s.st.UpsertUser(ctx, idt.Subject, claims.Email, name); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	sess := session{Sub: idt.Subject, Email: claims.Email, Name: name, Roles: roles, Exp: time.Now().Add(sessionTTL).Unix()}
	sealed, err := s.accounts.seal(sess)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.accounts.setCookie(w, r, sessionCookie, sealed, int(sessionTTL.Seconds()))
	log.Printf("accounts: signed in sub=%s email=%s host=%s", idt.Subject, claims.Email, hostOf(r))
	http.Redirect(w, r, safeNext(st.Next), http.StatusFound)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.accounts.clearCookie(w, r, sessionCookie)
	w.Header().Set("Cache-Control", "no-store")
	s.accounts.mu.RLock()
	end := s.accounts.endSession
	s.accounts.mu.RUnlock()
	home := "https://" + hostOf(r) + "/"
	if !secureRequest(r) {
		home = "http://" + r.Host + "/"
	}
	if end == "" || !s.accounts.enabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	q := url.Values{"client_id": {s.cfg.OIDCClientID}, "post_logout_redirect_uri": {home}}
	http.Redirect(w, r, end+"?"+q.Encode(), http.StatusFound)
}

// me returns the caller's identity as JSON (401 when anonymous).
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p, err := s.identify(r)
	if err != nil || !p.authed() {
		jsonErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	out := map[string]any{"kind": p.Kind, "via": p.Via}
	if p.isUser() {
		out["sub"], out["email"], out["name"], out["plan"] = p.Sub, p.Email, p.Name, p.Plan
		out["limits"] = s.accounts.limitsFor(p.Plan)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- per-user tokens API ------------------------------------------------------------------

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if !p.isUser() {
		jsonErr(w, http.StatusForbidden, "user tokens belong to signed-in users")
		return
	}
	ts, err := s.st.ListUserTokens(r.Context(), p.Sub)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if !p.isUser() {
		jsonErr(w, http.StatusForbidden, "user tokens belong to signed-in users")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)
	name := clip(req.Name, 60)
	if name == "" {
		name = "token"
	}
	ts, _ := s.st.ListUserTokens(r.Context(), p.Sub)
	if len(ts) >= 10 {
		jsonErr(w, http.StatusConflict, "at most 10 tokens per account — revoke one first")
		return
	}
	tok := newUserToken()
	id := randomID(10)
	if err := s.st.CreateUserToken(r.Context(), id, p.Sub, hashToken(tok), name); err != nil {
		jsonErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	log.Printf("accounts: token created sub=%s id=%s", p.Sub, id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": name, "token": tok, "created_at": time.Now().UTC()})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if !p.isUser() {
		jsonErr(w, http.StatusForbidden, "user tokens belong to signed-in users")
		return
	}
	switch err := s.st.DeleteUserToken(r.Context(), p.Sub, r.PathValue("id")); {
	case errors.Is(err, errNotFound):
		jsonErr(w, http.StatusNotFound, "no such token")
	case err != nil:
		jsonErr(w, http.StatusInternalServerError, "storage error")
	default:
		log.Printf("accounts: token revoked sub=%s id=%s", p.Sub, r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- account page ----------------------------------------------------------------------------

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	if !s.accounts.enabled() {
		http.NotFound(w, r)
		return
	}
	p, _ := s.identify(r)
	if !p.isUser() || p.Via != "session" {
		http.Redirect(w, r, "/login?next=/account", http.StatusFound)
		return
	}
	ts, _ := s.st.ListUserTokens(r.Context(), p.Sub)
	data := s.pageData(s.kindOf(r), r)
	data["Page"] = "account"
	data["Tokens"] = ts
	data["BillingURL"] = s.cfg.BillingAccountURL
	s.uiHeaders(w)
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "no-store")
	if err := tmpl.ExecuteTemplate(w, "account.html", data); err != nil {
		log.Printf("account page: %v", err)
	}
}

func (s *Server) kindOf(r *http.Request) string {
	if hostOf(r) == s.cfg.PasteHost {
		return "pastes"
	}
	return "links"
}

// userView is what templates get about the signed-in user (nil when anonymous).
type userView struct {
	Sub, Email, Name, Plan string
	Limits                 planLimits
	MaxPaste, MaxTTL       string
	Forever                bool
}

func (s *Server) userViewFor(r *http.Request) *userView {
	if !s.accounts.enabled() {
		return nil
	}
	sess := s.accounts.sessionFrom(r)
	if sess == nil {
		return nil
	}
	plan := s.accounts.planFor(r.Context(), sess.Sub)
	lim := s.accounts.limitsFor(plan)
	v := &userView{Sub: sess.Sub, Email: sess.Email, Name: sess.Name, Plan: plan, Limits: lim,
		MaxPaste: humanSize(lim.MaxPasteBytes), Forever: lim.MaxTTL == 0}
	if lim.MaxTTL == 0 {
		v.MaxTTL = "forever"
	} else {
		v.MaxTTL = humanTTL(lim.MaxTTL)
	}
	return v
}

func (a *accounts) String() string {
	if !a.enabled() {
		return "accounts=off"
	}
	return fmt.Sprintf("accounts=on issuer=%s", a.cfg.OIDCIssuer)
}
