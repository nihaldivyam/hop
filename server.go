package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html static/*
var assets embed.FS

var tmpl = template.Must(template.ParseFS(assets, "templates/*.html"))

// Server routes by Host header: one mux for the links host, one for the paste
// host. The write API and /healthz are mounted on both. Unknown hosts behave
// like the links host, which keeps local runs (localhost) usable.
type Server struct {
	cfg     Config
	st      *Store
	links   *http.ServeMux
	pastes  *http.ServeMux
	limiter *limiter
	// anonLimiter meters anonymous paste creation per client IP (public pastes mode).
	anonLimiter *limiter
	// anonLinkLimiter does the same for anonymous short links (public links mode).
	anonLinkLimiter *limiter
	// static assets (embedded) keyed by file name, plus a short content hash
	// used as ?v= in the templates so browsers can cache them hard but never stale.
	static   map[string][]byte
	assetVer string
	// accounts: OIDC sign-in, sessions, per-user tokens, plan limits (inert when OIDC_CLIENT_ID is empty)
	accounts *accounts
}

func newServer(cfg Config, st *Store) *Server {
	if cfg.Plans == nil {
		cfg.Plans = defaultPlans()
	}
	s := &Server{cfg: cfg, st: st, links: http.NewServeMux(), pastes: http.NewServeMux(),
		limiter: newLimiter(10, 30), static: map[string][]byte{}}
	s.accounts = newAccounts(cfg, st)
	if cfg.AnonRateN > 0 && cfg.AnonRatePer > 0 {
		s.anonLimiter = newLimiter(float64(cfg.AnonRateN)/cfg.AnonRatePer.Seconds(), float64(max(cfg.AnonBurst, 1)))
	} else {
		s.anonLimiter = newLimiter(5.0/3600, 2)
	}
	if cfg.AnonLinkRateN > 0 && cfg.AnonLinkRatePer > 0 {
		s.anonLinkLimiter = newLimiter(float64(cfg.AnonLinkRateN)/cfg.AnonLinkRatePer.Seconds(), float64(max(cfg.AnonLinkBurst, 1)))
	} else {
		s.anonLinkLimiter = newLimiter(5.0/3600, 2)
	}
	h := sha256.New()
	for _, name := range []string{"style.css", "app.js", "paste.css"} {
		b, err := assets.ReadFile("static/" + name)
		if err != nil {
			panic("embedded static/" + name + " missing: " + err.Error())
		}
		s.static[name] = b
		h.Write(b)
	}
	s.assetVer = fmt.Sprintf("%x", h.Sum(nil))[:10]
	// paste.css also carries the chroma stylesheet generated at startup
	s.static["paste.css"] = append(append(s.static["paste.css"], '\n'), highlightCSS()...)

	for _, m := range []*http.ServeMux{s.links, s.pastes} {
		m.HandleFunc("GET /healthz", s.healthz)
		// explicit paths, not /static/{name}: a wildcard there would conflict with GET /{id}/raw
		m.HandleFunc("GET /static/style.css", s.serveStatic("style.css"))
		m.HandleFunc("GET /static/app.js", s.serveStatic("app.js"))
		m.HandleFunc("GET /static/paste.css", s.serveStatic("paste.css"))
		m.Handle("GET /api/links", s.auth(s.listLinks))
		m.HandleFunc("POST /api/links", s.createLinkGate)
		m.Handle("DELETE /api/links", s.owner(s.purgeLinks))
		m.Handle("DELETE /api/links/{slug}", s.auth(s.deleteLink))
		m.Handle("GET /api/pastes", s.auth(s.listPastes))
		m.HandleFunc("POST /api/pastes", s.createPasteGate)
		m.Handle("DELETE /api/pastes", s.owner(s.purgePastes))
		m.Handle("DELETE /api/pastes/{id}", s.auth(s.deletePaste))
		// accounts (404 when OIDC is not configured)
		m.HandleFunc("GET /login", s.login)
		m.HandleFunc("GET /auth/callback", s.authCallback)
		m.HandleFunc("GET /logout", s.logout)
		m.HandleFunc("GET /me", s.me)
		m.HandleFunc("GET /account", s.accountPage)
		m.Handle("GET /api/tokens", s.auth(s.listTokens))
		m.Handle("POST /api/tokens", s.auth(s.createToken))
		m.Handle("DELETE /api/tokens/{id}", s.auth(s.deleteToken))
	}
	s.links.HandleFunc("GET /{$}", s.landing("links"))
	s.links.Handle("GET /{slug}", s.limit(s.redirect))
	s.links.Handle("GET /{slug}/go", s.limit(s.redirectGo))
	s.pastes.HandleFunc("GET /{$}", s.landing("pastes"))
	s.pastes.Handle("GET /{id}", s.limit(s.showPaste))
	s.pastes.Handle("GET /{id}/raw", s.limit(s.rawPaste))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if hostOf(r) == s.cfg.PasteHost {
		s.pastes.ServeHTTP(w, r)
		return
	}
	s.links.ServeHTTP(w, r)
}

// Start kicks off background work (OIDC discovery with retries). Safe to skip in tests.
func (s *Server) Start(ctx context.Context) { s.accounts.start(ctx) }

func hostOf(r *http.Request) string {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.ToLower(h)
}

// --- middleware ---------------------------------------------------------------

// auth admits the owner token, a per-user token, or a signed-in session (the
// latter only with the same-origin CSRF header on state-changing requests) and
// puts the principal into the request context. Handlers scope by principal.
func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" && !s.accounts.enabled() {
			jsonErr(w, http.StatusServiceUnavailable, "writes disabled: HOP_TOKEN is not set")
			return
		}
		p, err := s.identify(r)
		if err != nil || !p.authed() {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hop"`)
			jsonErr(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if p.Via == "session" && r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			jsonErr(w, http.StatusForbidden, "cross-site request refused (send X-Requested-With: hop)")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

// owner admits only the HOP_TOKEN holder (purges, instance-wide operations).
func (s *Server) owner(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" {
			jsonErr(w, http.StatusServiceUnavailable, "writes disabled: HOP_TOKEN is not set")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == r.Header.Get("Authorization") || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hop"`)
			jsonErr(w, http.StatusUnauthorized, "this operation needs the owner token")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalKey, principal{Kind: "owner", Via: "token"})))
	})
}

func (s *Server) limit(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(s.clientIP(r)) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// --- public pages -------------------------------------------------------------

// landing: the two front pages. They carry a tiny in-browser UI (static/app.js)
// that talks to this origin's /api/* with a token kept in localStorage, so the
// CSP allows scripts/styles/fetch from 'self' only — the paste view keeps the
// stricter no-script policy in htmlHeaders.
func (s *Server) landing(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.uiHeaders(w)
		if err := tmpl.ExecuteTemplate(w, "landing.html", s.pageData(kind, r)); err != nil {
			log.Printf("landing: %v", err)
		}
	}
}

// uiHeaders: the headers for pages that carry the in-browser UI (landing and the
// "create this paste" page): scripts/styles/fetch from 'self' only, never cached.
func (s *Server) uiHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-cache")
}

// pageData is the template data shared by the landing pages, the create page and
// the account page. r is used for the signed-in user (nil when anonymous).
func (s *Server) pageData(kind string, r *http.Request) map[string]any {
	var user *userView
	if r != nil {
		user = s.userViewFor(r)
	}
	return map[string]any{
		// accounts
		"Accounts": s.accounts.enabled(), "User": user, "Page": "",
		"Kind": kind, "Repo": s.cfg.RepoURL, "LinksHost": s.cfg.LinksHost, "PasteHost": s.cfg.PasteHost,
		"MaxPaste": humanSize(s.cfg.MaxPasteBytes), "DefaultTTL": humanTTL(s.cfg.DefaultPasteTTL),
		"AssetVer": s.assetVer, "WritesEnabled": s.cfg.Token != "",
		// anonymous pastes (paste host only)
		"PublicPastes": kind == "pastes" && s.cfg.PublicPastes,
		"AnonMaxBytes": s.cfg.AnonMaxBytes, "AnonMax": humanSize(s.cfg.AnonMaxBytes),
		"AnonTTL": humanDur(s.cfg.AnonMaxTTL), "AnonTTLRaw": s.cfg.AnonMaxTTL.String(),
		// anonymous links (links host only)
		"PublicLinks": kind == "links" && s.cfg.PublicLinks,
		"AnonLinkTTL": humanDur(s.cfg.AnonLinkMaxTTL), "AnonLinkInterstitial": s.cfg.AnonLinkInterstitial,
		"Public": (kind == "pastes" && s.cfg.PublicPastes) || (kind == "links" && s.cfg.PublicLinks),
		// name-your-own-URL rules (paste host)
		"IDRule": pasteIDRule, "IDPattern": pasteIDRe.String()[1 : len(pasteIDRe.String())-1],
	}
}

// createPage is what a browser gets for GET /{name} on the paste host when no
// such paste exists and the name is a valid custom id: a 200 editor that
// creates the paste AT that name (app.js POSTs with X-Id and then navigates to
// it). Invalid names get a 404 page with the rule. Non-browsers get plain 404.
func (s *Server) createPage(w http.ResponseWriter, r *http.Request, name string) {
	data := s.pageData("pastes", r)
	data["CreateID"] = name
	data["Invalid"] = !validPasteID(name)
	s.uiHeaders(w)
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if data["Invalid"] == true {
		w.WriteHeader(http.StatusNotFound)
	}
	if err := tmpl.ExecuteTemplate(w, "create.html", data); err != nil {
		log.Printf("create page: %v", err)
	}
}

// serveStatic hands out one embedded asset. Requests carrying the current
// ?v=<hash> are cached for a year; anything else must revalidate (ETag), so a
// redeploy never leaves a browser with a stale stylesheet.
func (s *Server) serveStatic(name string) http.HandlerFunc {
	b := s.static[name]
	ctype := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".css"):
		ctype = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		ctype = "text/javascript; charset=utf-8"
	}
	etag := `"` + s.assetVer + `"`
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("ETag", etag)
		if r.URL.Query().Get("v") == s.assetVer {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write(b)
	}
}

func humanTTL(d time.Duration) string {
	switch {
	case d == 0:
		return "never"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	default:
		return d.String()
	}
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.st.Ping(ctx); err != nil {
		http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}

// redirect serves GET /{slug}. Token-created links always 302. Anonymous links
// 302 as well, except for browsers (Accept: text/html) when the interstitial
// is on: they get a 200 confirmation page that names the destination and
// links to /{slug}/go — a human sees where an anonymous link leads before
// following it; curl/bots are not the concern and go straight through.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	l, err := s.st.GetLink(r.Context(), slug)
	if errors.Is(err, errNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if l.Anon && s.cfg.AnonLinkInterstitial && strings.Contains(r.Header.Get("Accept"), "text/html") {
		s.interstitial(w, r, l)
		return
	}
	s.follow(w, r, l)
}

// redirectGo serves GET /{slug}/go — the "Continue" target of the interstitial.
func (s *Server) redirectGo(w http.ResponseWriter, r *http.Request) {
	l, err := s.st.GetLink(r.Context(), r.PathValue("slug"))
	if errors.Is(err, errNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	s.follow(w, r, l)
}

// interstitial renders the confirmation page for an anonymous link: no scripts,
// strict CSP, not indexed, no referrer leaks, never cached.
func (s *Server) interstitial(w http.ResponseWriter, r *http.Request, l *Link) {
	u, _ := url.Parse(l.URL)
	host := ""
	if u != nil {
		host = u.Host
	}
	expires := "never"
	if l.ExpiresAt != nil {
		expires = "in " + humanDur(time.Until(*l.ExpiresAt)) + " (" + l.ExpiresAt.Format("2006-01-02") + ")"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "no-store")
	data := map[string]any{
		"Slug": l.Slug, "URL": l.URL, "Host": host, "GoURL": "/" + l.Slug + "/go",
		"Created": humanDur(time.Since(l.CreatedAt)), "Expires": expires,
		"AssetVer": s.assetVer, "Repo": s.cfg.RepoURL, "LinksHost": s.cfg.LinksHost,
		"Accounts": s.accounts.enabled(), "User": s.userViewFor(r),
	}
	if err := tmpl.ExecuteTemplate(w, "interstitial.html", data); err != nil {
		log.Printf("interstitial: %v", err)
	}
}

// follow performs the actual redirect and counts the hit.
func (s *Server) follow(w http.ResponseWriter, r *http.Request, l *Link) {
	slug := l.Slug
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.st.TouchLink(ctx, slug); err != nil {
			log.Printf("touch %s: %v", slug, err)
		}
	}()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, l.URL, http.StatusFound)
}

func (s *Server) htmlHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
}

// showPaste: text/plain unless the client asks for HTML (browser Accept,
// ?html=1, or an extension like /abc123.go). The HTML view never trusts content.
func (s *Server) showPaste(w http.ResponseWriter, r *http.Request) {
	id, ext := r.PathValue("id"), ""
	if i := strings.LastIndexByte(id, '.'); i > 0 {
		id, ext = id[:i], id[i+1:]
	}
	p, err := s.st.GetPaste(r.Context(), id)
	if errors.Is(err, errNotFound) {
		// "name your own URL": a browser landing on a free name gets an editor for it
		if ext == "" && strings.Contains(r.Header.Get("Accept"), "text/html") {
			s.createPage(w, r, id)
			return
		}
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	wantHTML := ext != "" || r.URL.Query().Get("html") == "1" || strings.Contains(r.Header.Get("Accept"), "text/html")
	if !wantHTML {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(p.Content)
		return
	}
	lang := p.Lang
	if ext != "" {
		lang = ext
	}
	lexer := pickLexer(lang, p.Title, p.Content)
	code, err := renderCode(lexer, p.Content)
	if err != nil {
		log.Printf("paste highlight %s: %v", p.ID, err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	plain := string(p.Content)
	nLines := strings.Count(strings.TrimRight(plain, "\n"), "\n") + 1
	expires := "never"
	if p.ExpiresAt != nil {
		expires = "in " + humanDur(time.Until(*p.ExpiresAt)) + " (" + p.ExpiresAt.Format("2006-01-02") + ")"
	}
	s.htmlHeaders(w)
	data := map[string]any{
		"ID": p.ID, "Title": p.Title, "Lang": langName(lexer), "Size": humanSize(p.Size), "Lines": nLines,
		"Created": p.CreatedAt.Format("2006-01-02 15:04 UTC"), "Expires": expires,
		"RawURL": "/" + p.ID + "/raw", "DownloadURL": "/" + p.ID + "/raw?dl=1",
		"Code": code, "Plain": plain, "PlainRows": min(max(nLines, 3), 20), "Repo": s.cfg.RepoURL,
		"Anon": p.Anon, "Accounts": s.accounts.enabled(), "User": s.userViewFor(r),
	}
	if err := tmpl.ExecuteTemplate(w, "paste.html", data); err != nil {
		log.Printf("paste view: %v", err)
	}
}

func (s *Server) rawPaste(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetPaste(r.Context(), r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("dl") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName(p, p.Lang)))
	} else {
		w.Header().Set("Content-Disposition", "inline")
	}
	w.Write(p.Content)
}

// --- write API ----------------------------------------------------------------

const maxBody = 1 << 20 // writes never need more than this

type linkReq struct {
	URL  string `json:"url"`
	Slug string `json:"slug"`
	TTL  string `json:"ttl"`
}

// createLinkGate decides between the token path and the anonymous path.
// Anonymous creates exist only when HOP_PUBLIC_LINKS is on, only on the links
// host, and only when the request carries no Authorization header at all — a
// wrong token is still rejected with 401 rather than silently downgraded.
func (s *Server) createLinkGate(w http.ResponseWriter, r *http.Request) {
	p, err := s.identify(r)
	if err == nil && p.authed() {
		s.auth(s.createLink).ServeHTTP(w, r)
		return
	}
	if r.Header.Get("Authorization") == "" && s.cfg.PublicLinks && hostOf(r) == s.cfg.LinksHost {
		s.createLinkAnon(w, r)
		return
	}
	s.auth(s.createLink).ServeHTTP(w, r) // yields the 401/503
}

// createLink is the authenticated path: owner token (unlimited) or a signed-in
// user (plan limits): custom slugs, direct redirects, TTL 0 allowed where the plan allows.
func (s *Server) createLink(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.isUser() {
		if !s.userQuota(w, r, p) {
			return
		}
	}
	s.storeLink(w, r, p, false, "")
}

// createLinkAnon is the public path: random slug only, short-lived, rate-limited,
// stricter URL rules, and (by default) a confirmation page before the redirect.
func (s *Server) createLinkAnon(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if ok, wait := s.anonLinkLimiter.allowWait(ip); !ok {
		secs := int(wait.Seconds()) + 1
		log.Printf("anon link rate-limited ip=%s retry_after=%ds", ip, secs)
		w.Header().Set("Retry-After", fmt.Sprint(secs))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limited", "retry_after_seconds": secs})
		return
	}
	if s.cfg.AnonLinkDailyCap >= 0 {
		day := time.Now().UTC().Truncate(24 * time.Hour)
		n, err := s.st.CountAnonLinksSince(r.Context(), day)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		if n >= s.cfg.AnonLinkDailyCap {
			log.Printf("anon link daily cap reached ip=%s cap=%d", ip, s.cfg.AnonLinkDailyCap)
			jsonErr(w, http.StatusTooManyRequests, "daily cap reached")
			return
		}
	}
	s.storeLink(w, r, principal{Kind: "anon"}, true, ip)
}

// anonTargetOK applies the stricter destination rules for anonymous links:
// absolute http(s), no credentials in the URL, no private/loopback/link-local
// IP literals or localhost (no pointing the public redirector at internal
// things), not this service itself (no redirect chains), ≤ 2048 chars.
func (s *Server) anonTargetOK(u *url.URL) (string, bool) {
	if len(u.String()) > 2048 {
		return "url longer than 2048 characters", false
	}
	if u.User != nil {
		return "url must not carry credentials (user:pass@)", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "url must be absolute http(s)", false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == s.cfg.LinksHost || host == s.cfg.PasteHost {
		return "that destination is not allowed for anonymous links", false
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return "that destination is not allowed for anonymous links", false
		}
	}
	return "", true
}

// storeLink does the shared work of all paths.
func (s *Server) storeLink(w http.ResponseWriter, r *http.Request, p principal, anon bool, ip string) {
	var req linkReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "body must be JSON {url, slug?, ttl?}")
		return
	}
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		jsonErr(w, http.StatusBadRequest, "url must be absolute http(s)")
		return
	}
	if anon {
		if msg, ok := s.anonTargetOK(u); !ok {
			jsonErr(w, http.StatusBadRequest, msg)
			return
		}
		if req.Slug != "" {
			jsonErr(w, http.StatusBadRequest, "custom slugs need the token; anonymous links get a random slug")
			return
		}
	}
	l := &Link{URL: u.String(), CreatedAt: time.Now().UTC(), Anon: anon, OwnerSub: p.ownerScope()}
	if anon {
		l.IP = ip
	}
	ttl := time.Duration(0)
	if anon {
		ttl = s.cfg.AnonLinkMaxTTL
	}
	var plan planLimits
	if p.isUser() {
		plan = s.accounts.limitsFor(p.Plan)
	}
	if req.TTL != "" {
		d, err := parseTTL(req.TTL)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ttl = d
	}
	if anon && (ttl <= 0 || ttl > s.cfg.AnonLinkMaxTTL) {
		ttl = s.cfg.AnonLinkMaxTTL // anonymous links always expire, at most after AnonLinkMaxTTL
	}
	if p.isUser() && plan.MaxTTL > 0 && (ttl <= 0 || ttl > plan.MaxTTL) {
		ttl = plan.MaxTTL // the free plan cannot make permanent links
	}
	if ttl > 0 {
		t := l.CreatedAt.Add(ttl)
		l.ExpiresAt = &t
	}
	if req.Slug != "" {
		if !validSlug(req.Slug) {
			jsonErr(w, http.StatusBadRequest, "slug must match [A-Za-z0-9_-]{1,64} and not be reserved")
			return
		}
		l.Slug = req.Slug
		if err := s.st.CreateLink(r.Context(), l); errors.Is(err, errExists) {
			jsonErr(w, http.StatusConflict, "slug already taken")
			return
		} else if err != nil {
			jsonErr(w, http.StatusInternalServerError, "storage error")
			return
		}
	} else {
		// random slug; on the (rare) collision grow the length.
		// Anonymous links get one extra character so they are distinguishable.
		n := 5
		if anon {
			n = 6
		}
		for attempt := 0; ; attempt++ {
			l.Slug = randomID(n)
			err := s.st.CreateLink(r.Context(), l)
			if err == nil {
				break
			}
			if !errors.Is(err, errExists) {
				jsonErr(w, http.StatusInternalServerError, "storage error")
				return
			}
			if attempt%3 == 2 {
				n++
			}
		}
	}
	if anon {
		log.Printf("anon link slug=%s ip=%s target_host=%s ttl=%s", l.Slug, ip, u.Host, ttl)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"slug": l.Slug, "short_url": "https://" + s.cfg.LinksHost + "/" + l.Slug,
		"url": l.URL, "expires_at": l.ExpiresAt, "anon": anon, "owned": p.isUser(),
	})
}

// purgeLinks removes every anonymous link at once (DELETE /api/links?anon=1).
func (s *Server) purgeLinks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("anon") != "1" {
		jsonErr(w, http.StatusBadRequest, "use DELETE /api/links?anon=1 to purge anonymous links, or DELETE /api/links/{slug}")
		return
	}
	n, err := s.st.PurgeAnonLinks(r.Context())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	log.Printf("anon links purged count=%d", n)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

func (s *Server) deleteLink(w http.ResponseWriter, r *http.Request) {
	switch err := s.st.DeleteLink(r.Context(), r.PathValue("slug"), principalFrom(r.Context()).ownerScope()); {
	case errors.Is(err, errNotFound):
		jsonErr(w, http.StatusNotFound, "no such slug")
	case err != nil:
		jsonErr(w, http.StatusInternalServerError, "storage error")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) listLinks(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	ls, err := s.st.ListLinks(r.Context(), p.ownerScope())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	// same shape as the create response, so clients never have to guess the host
	out := make([]map[string]any, 0, len(ls))
	for _, l := range ls {
		row := map[string]any{
			"slug": l.Slug, "short_url": "https://" + s.cfg.LinksHost + "/" + l.Slug, "url": l.URL,
			"created_at": l.CreatedAt, "expires_at": l.ExpiresAt, "hits": l.Hits, "last_used": l.LastUsed, "anon": l.Anon,
		}
		if l.Anon && p.isOwner() {
			row["ip"] = l.IP // the owner sees who created anonymous links; nobody else does
		}
		if l.OwnerSub != "" && p.isOwner() {
			row["owner_sub"] = l.OwnerSub
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// createPasteGate decides between the token path and the anonymous path.
// Anonymous creates exist only when HOP_PUBLIC_PASTES is on, only on the paste
// host, and only when the request carries no Authorization header at all — a
// wrong token is still rejected with 401 rather than silently downgraded.
func (s *Server) createPasteGate(w http.ResponseWriter, r *http.Request) {
	p, err := s.identify(r)
	if err == nil && p.authed() {
		s.auth(s.createPaste).ServeHTTP(w, r)
		return
	}
	if r.Header.Get("Authorization") == "" && s.cfg.PublicPastes && hostOf(r) == s.cfg.PasteHost {
		s.createPasteAnon(w, r)
		return
	}
	s.auth(s.createPaste).ServeHTTP(w, r) // yields the 401/503
}

// createPaste is the authenticated path: the owner token gets the instance
// limits (TTL 0 allowed); a signed-in user gets their plan's limits.
func (s *Server) createPaste(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.isUser() {
		if !s.userQuota(w, r, p) {
			return
		}
		pl := s.accounts.limitsFor(p.Plan)
		s.storePaste(w, r, p, false, pl.MaxPasteBytes, pl.DefaultPasteTTL, "")
		return
	}
	s.storePaste(w, r, p, false, s.cfg.MaxPasteBytes, s.cfg.DefaultPasteTTL, "")
}

// userQuota applies a signed-in user's rate limit and item cap; false = response written.
func (s *Server) userQuota(w http.ResponseWriter, r *http.Request, p principal) bool {
	pl := s.accounts.limitsFor(p.Plan)
	if lim := s.accounts.limits[pl.Name]; lim != nil {
		if ok, wait := lim.allowWait(p.Sub); !ok {
			secs := int(wait.Seconds()) + 1
			w.Header().Set("Retry-After", fmt.Sprint(secs))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limited for the " + pl.Name + " plan", "retry_after_seconds": secs})
			return false
		}
	}
	if pl.MaxItems > 0 {
		n, err := s.st.CountOwned(r.Context(), p.Sub)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "storage error")
			return false
		}
		if n >= pl.MaxItems {
			jsonErr(w, http.StatusTooManyRequests, fmt.Sprintf("the %s plan holds at most %d links + pastes — delete some or upgrade", pl.Name, pl.MaxItems))
			return false
		}
	}
	return true
}

// createPasteAnon is the public path: small, short-lived, rate-limited, text only.
func (s *Server) createPasteAnon(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if ok, wait := s.anonLimiter.allowWait(ip); !ok {
		secs := int(wait.Seconds()) + 1
		log.Printf("anon paste rate-limited ip=%s retry_after=%ds", ip, secs)
		w.Header().Set("Retry-After", fmt.Sprint(secs))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limited", "retry_after_seconds": secs})
		return
	}
	if s.cfg.AnonDailyCap >= 0 {
		day := time.Now().UTC().Truncate(24 * time.Hour)
		n, err := s.st.CountAnonSince(r.Context(), day)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		if n >= s.cfg.AnonDailyCap {
			log.Printf("anon paste daily cap reached ip=%s cap=%d", ip, s.cfg.AnonDailyCap)
			jsonErr(w, http.StatusTooManyRequests, "daily cap reached")
			return
		}
	}
	s.storePaste(w, r, principal{Kind: "anon"}, true, s.cfg.AnonMaxBytes, s.cfg.AnonMaxTTL, ip)
}

// storePaste does the shared work. For anonymous pastes the TTL is clamped to
// maxTTL (and "forever" is not available), binary content is refused, and the
// creator IP is recorded for abuse handling.
func (s *Server) storePaste(w http.ResponseWriter, r *http.Request, p principal, anon bool, limit int64, defaultTTL time.Duration, ip string) {
	body, title, lang, err := readPasteBody(r, limit)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			msg := fmt.Sprintf("paste larger than %d bytes", limit)
			if anon {
				msg = fmt.Sprintf("anonymous pastes are limited to %d bytes (%s); unlock with the token for more", limit, humanSize(limit))
			} else if p.isUser() {
				msg = fmt.Sprintf("the %s plan allows pastes up to %s", p.Plan, humanSize(limit))
			}
			jsonErr(w, http.StatusRequestEntityTooLarge, msg)
			return
		}
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body) == 0 {
		jsonErr(w, http.StatusBadRequest, "empty paste")
		return
	}
	if anon && bytes.IndexByte(body, 0) >= 0 {
		jsonErr(w, http.StatusUnsupportedMediaType, "anonymous pastes must be text (binary content refused)")
		return
	}
	ttl := defaultTTL
	if v := r.Header.Get("X-TTL"); v != "" {
		if ttl, err = parseTTL(v); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if anon && (ttl <= 0 || ttl > s.cfg.AnonMaxTTL) {
		ttl = s.cfg.AnonMaxTTL // anonymous pastes always expire, at most after AnonMaxTTL
	}
	if p.isUser() {
		if pl := s.accounts.limitsFor(p.Plan); pl.MaxTTL > 0 && (ttl <= 0 || ttl > pl.MaxTTL) {
			ttl = pl.MaxTTL // the free plan cannot keep pastes forever
		}
	}
	pst := &Paste{Title: clip(title, 120), Lang: clip(lang, 20), Content: body, Size: int64(len(body)),
		CreatedAt: time.Now().UTC(), Anon: anon, OwnerSub: p.ownerScope()}
	if anon {
		pst.IP = ip
	}
	if ttl > 0 {
		t := pst.CreatedAt.Add(ttl)
		pst.ExpiresAt = &t
	}
	// optional custom id ("name your own URL"): X-Id header or ?id= query
	custom := strings.TrimSpace(r.Header.Get("X-Id"))
	if custom == "" {
		custom = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if custom != "" {
		if !validPasteID(custom) {
			jsonErr(w, http.StatusBadRequest, "invalid id: "+pasteIDRule)
			return
		}
		pst.ID = custom
		if err := s.st.CreatePaste(r.Context(), pst); errors.Is(err, errExists) {
			jsonErr(w, http.StatusConflict, "id taken")
			return
		} else if err != nil {
			jsonErr(w, http.StatusInternalServerError, "storage error")
			return
		}
	} else {
		for attempt := 0; ; attempt++ {
			pst.ID = randomID(8)
			err := s.st.CreatePaste(r.Context(), pst)
			if err == nil {
				break
			}
			if !errors.Is(err, errExists) || attempt > 5 {
				jsonErr(w, http.StatusInternalServerError, "storage error")
				return
			}
		}
	}
	if anon {
		log.Printf("anon paste id=%s ip=%s bytes=%d ttl=%s custom=%t", pst.ID, ip, pst.Size, ttl, custom != "")
	}
	base := "https://" + s.cfg.PasteHost + "/" + pst.ID
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": pst.ID, "url": base, "raw_url": base + "/raw", "expires_at": pst.ExpiresAt, "size": pst.Size, "anon": anon, "owned": p.isUser(),
	})
}

// purgePastes removes every anonymous paste at once (DELETE /api/pastes?anon=1).
func (s *Server) purgePastes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("anon") != "1" {
		jsonErr(w, http.StatusBadRequest, "use DELETE /api/pastes?anon=1 to purge anonymous pastes, or DELETE /api/pastes/{id}")
		return
	}
	n, err := s.st.PurgeAnon(r.Context())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	log.Printf("anon pastes purged count=%d", n)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// readPasteBody accepts raw bodies (any text/* or octet-stream) and multipart
// forms (field "file" or "content"). Title/lang come from X-Title / X-Lang or
// the multipart filename.
func readPasteBody(r *http.Request, limit int64) (body []byte, title, lang string, err error) {
	r.Body = http.MaxBytesReader(nil, r.Body, limit+64<<10) // multipart overhead
	title, lang = r.Header.Get("X-Title"), r.Header.Get("X-Lang")
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err = r.ParseMultipartForm(limit); err != nil {
			return nil, "", "", err
		}
		for _, field := range []string{"file", "content"} {
			if f, hdr, e := r.FormFile(field); e == nil {
				defer f.Close()
				if title == "" {
					title = hdr.Filename
				}
				body, err = io.ReadAll(io.LimitReader(f, limit+1))
				break
			}
		}
		if body == nil {
			if v := r.FormValue("content"); v != "" {
				body = []byte(v)
			}
		}
	} else {
		body, err = io.ReadAll(r.Body)
	}
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(body)) > limit {
		return nil, "", "", &http.MaxBytesError{Limit: limit}
	}
	return body, title, lang, nil
}

func (s *Server) deletePaste(w http.ResponseWriter, r *http.Request) {
	switch err := s.st.DeletePaste(r.Context(), r.PathValue("id"), principalFrom(r.Context()).ownerScope()); {
	case errors.Is(err, errNotFound):
		jsonErr(w, http.StatusNotFound, "no such paste")
	case err != nil:
		jsonErr(w, http.StatusInternalServerError, "storage error")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) listPastes(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	ps, err := s.st.ListPastes(r.Context(), p.ownerScope())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		base := "https://" + s.cfg.PasteHost + "/" + p.ID
		row := map[string]any{
			"id": p.ID, "title": p.Title, "lang": p.Lang, "size": p.Size, "url": base, "raw_url": base + "/raw",
			"created_at": p.CreatedAt, "expires_at": p.ExpiresAt, "anon": p.Anon,
		}
		if p.Anon && principalFrom(r.Context()).isOwner() {
			row["ip"] = p.IP // the owner sees who created anonymous pastes; the public view never does
		}
		if p.OwnerSub != "" && principalFrom(r.Context()).isOwner() {
			row["owner_sub"] = p.OwnerSub
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- small helpers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// --- a tiny per-IP token bucket for anonymous reads -----------------------------

type limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	b      map[string]*bucket
	lastGC time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, b: map[string]*bucket{}, lastGC: time.Now()}
}

func (l *limiter) allow(key string) bool {
	ok, _ := l.allowWait(key)
	return ok
}

// allowWait is allow plus, on refusal, how long until the next token arrives.
func (l *limiter) allowWait(key string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) > 5*time.Minute {
		for k, v := range l.b {
			if now.Sub(v.last) > 5*time.Minute {
				delete(l.b, k)
			}
		}
		l.lastGC = now
	}
	bk, ok := l.b[key]
	if !ok {
		bk = &bucket{tokens: l.burst, last: now}
		l.b[key] = bk
	}
	bk.tokens = min(l.burst, bk.tokens+now.Sub(bk.last).Seconds()*l.rate)
	bk.last = now
	if bk.tokens < 1 {
		wait := time.Duration((1 - bk.tokens) / l.rate * float64(time.Second))
		return false, wait
	}
	bk.tokens--
	return true, 0
}
