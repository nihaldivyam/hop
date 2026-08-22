package main

import (
	"context"
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

//go:embed templates/*.html static/style.css
var assets embed.FS

var tmpl = template.Must(template.ParseFS(assets, "templates/*.html"))

// Server routes by Host header: one mux for the links host, one for the paste
// host. The write API and /healthz are mounted on both. Unknown hosts behave
// like the links host, which keeps local runs (localhost) usable.
type Server struct {
	cfg      Config
	st       *Store
	links    *http.ServeMux
	pastes   *http.ServeMux
	limiter  *limiter
	styleCSS []byte
}

func newServer(cfg Config, st *Store) *Server {
	css, _ := assets.ReadFile("static/style.css")
	s := &Server{cfg: cfg, st: st, links: http.NewServeMux(), pastes: http.NewServeMux(),
		limiter: newLimiter(10, 30), styleCSS: css}

	for _, m := range []*http.ServeMux{s.links, s.pastes} {
		m.HandleFunc("GET /healthz", s.healthz)
		m.HandleFunc("GET /static/style.css", s.static)
		m.Handle("GET /api/links", s.auth(s.listLinks))
		m.Handle("POST /api/links", s.auth(s.createLink))
		m.Handle("DELETE /api/links/{slug}", s.auth(s.deleteLink))
		m.Handle("GET /api/pastes", s.auth(s.listPastes))
		m.Handle("POST /api/pastes", s.auth(s.createPaste))
		m.Handle("DELETE /api/pastes/{id}", s.auth(s.deletePaste))
	}
	s.links.HandleFunc("GET /{$}", s.landing("links"))
	s.links.Handle("GET /{slug}", s.limit(s.redirect))
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

func hostOf(r *http.Request) string {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.ToLower(h)
}

// --- middleware ---------------------------------------------------------------

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" {
			jsonErr(w, http.StatusServiceUnavailable, "writes disabled: HOP_TOKEN is not set")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == r.Header.Get("Authorization") || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hop"`)
			jsonErr(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		h(w, r)
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

func (s *Server) landing(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.htmlHeaders(w)
		data := map[string]any{"Kind": kind, "Repo": s.cfg.RepoURL, "LinksHost": s.cfg.LinksHost, "PasteHost": s.cfg.PasteHost}
		if err := tmpl.ExecuteTemplate(w, "landing.html", data); err != nil {
			log.Printf("landing: %v", err)
		}
	}
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(s.styleCSS)
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
	s.htmlHeaders(w)
	lines := strings.Split(strings.TrimRight(string(p.Content), "\n"), "\n")
	data := map[string]any{
		"P": p, "Lang": lang, "Lines": lines, "Repo": s.cfg.RepoURL,
		"RawURL": "/" + p.ID + "/raw", "Size": humanSize(p.Size),
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
	w.Header().Set("Content-Disposition", "inline")
	w.Write(p.Content)
}

// --- write API ----------------------------------------------------------------

const maxBody = 1 << 20 // writes never need more than this

type linkReq struct {
	URL  string `json:"url"`
	Slug string `json:"slug"`
	TTL  string `json:"ttl"`
}

func (s *Server) createLink(w http.ResponseWriter, r *http.Request) {
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
	l := &Link{URL: u.String(), CreatedAt: time.Now().UTC()}
	if req.TTL != "" {
		d, err := parseTTL(req.TTL)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if d > 0 {
			t := l.CreatedAt.Add(d)
			l.ExpiresAt = &t
		}
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
		// random slug; on the (rare) collision grow the length
		n := 5
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
	writeJSON(w, http.StatusCreated, map[string]any{
		"slug": l.Slug, "short_url": "https://" + s.cfg.LinksHost + "/" + l.Slug,
		"url": l.URL, "expires_at": l.ExpiresAt,
	})
}

func (s *Server) deleteLink(w http.ResponseWriter, r *http.Request) {
	switch err := s.st.DeleteLink(r.Context(), r.PathValue("slug")); {
	case errors.Is(err, errNotFound):
		jsonErr(w, http.StatusNotFound, "no such slug")
	case err != nil:
		jsonErr(w, http.StatusInternalServerError, "storage error")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) listLinks(w http.ResponseWriter, r *http.Request) {
	ls, err := s.st.ListLinks(r.Context())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	writeJSON(w, http.StatusOK, ls)
}

func (s *Server) createPaste(w http.ResponseWriter, r *http.Request) {
	limit := s.cfg.MaxPasteBytes
	body, title, lang, err := readPasteBody(r, limit)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("paste larger than %d bytes", limit))
			return
		}
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body) == 0 {
		jsonErr(w, http.StatusBadRequest, "empty paste")
		return
	}
	ttl := s.cfg.DefaultPasteTTL
	if v := r.Header.Get("X-TTL"); v != "" {
		if ttl, err = parseTTL(v); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	p := &Paste{Title: clip(title, 120), Lang: clip(lang, 20), Content: body, Size: int64(len(body)), CreatedAt: time.Now().UTC()}
	if ttl > 0 {
		t := p.CreatedAt.Add(ttl)
		p.ExpiresAt = &t
	}
	for attempt := 0; ; attempt++ {
		p.ID = randomID(8)
		err := s.st.CreatePaste(r.Context(), p)
		if err == nil {
			break
		}
		if !errors.Is(err, errExists) || attempt > 5 {
			jsonErr(w, http.StatusInternalServerError, "storage error")
			return
		}
	}
	base := "https://" + s.cfg.PasteHost + "/" + p.ID
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": p.ID, "url": base, "raw_url": base + "/raw", "expires_at": p.ExpiresAt, "size": p.Size,
	})
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
	switch err := s.st.DeletePaste(r.Context(), r.PathValue("id")); {
	case errors.Is(err, errNotFound):
		jsonErr(w, http.StatusNotFound, "no such paste")
	case err != nil:
		jsonErr(w, http.StatusInternalServerError, "storage error")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) listPastes(w http.ResponseWriter, r *http.Request) {
	ps, err := s.st.ListPastes(r.Context())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	writeJSON(w, http.StatusOK, ps)
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
		return false
	}
	bk.tokens--
	return true
}
