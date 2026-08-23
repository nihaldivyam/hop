package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is read once from the environment; every field has a sane default so
// `hop` with no env at all runs a local instance on :8090 with ./hop.db.
type Config struct {
	Listen          string        // HOP_LISTEN
	DBPath          string        // HOP_DB
	Token           string        // HOP_TOKEN — bearer token for all writes; empty disables writes
	LinksHost       string        // HOP_LINKS_HOST — host that serves short links
	PasteHost       string        // HOP_PASTE_HOST — host that serves pastes
	MaxPasteBytes   int64         // HOP_MAX_PASTE_BYTES
	DefaultPasteTTL time.Duration // HOP_DEFAULT_PASTE_TTL (0 = forever)
	RepoURL         string        // HOP_REPO_URL — shown on the landing pages
	TrustProxy      bool          // HOP_TRUST_PROXY — use X-Forwarded-For for client IPs

	// Anonymous pastes (off by default). When on, POST /api/pastes on the paste
	// host works without a token, inside hard limits; links always need the token.
	PublicPastes bool          // HOP_PUBLIC_PASTES
	AnonMaxBytes int64         // HOP_ANON_MAX_BYTES
	AnonMaxTTL   time.Duration // HOP_ANON_MAX_TTL — anonymous pastes always expire (requested TTL is clamped)
	AnonRateN    int           // HOP_ANON_RATE "N/period": N anonymous creates per period per client IP…
	AnonRatePer  time.Duration //   …with a burst of AnonBurst
	AnonBurst    int           // HOP_ANON_BURST
	AnonDailyCap int64         // HOP_ANON_DAILY_CAP — global anonymous pastes per UTC day

	// Anonymous short links (off by default). When on, POST /api/links on the
	// links host works without a token: random slug only, short-lived, rate
	// limited, and (by default) served through a confirmation page first.
	PublicLinks          bool          // HOP_PUBLIC_LINKS
	AnonLinkMaxTTL       time.Duration // HOP_ANON_LINK_MAX_TTL — anonymous links always expire (requested TTL is clamped)
	AnonLinkRateN        int           // HOP_ANON_LINK_RATE "N/period" per client IP…
	AnonLinkRatePer      time.Duration //   …with a burst of AnonLinkBurst
	AnonLinkBurst        int           // HOP_ANON_LINK_BURST
	AnonLinkDailyCap     int64         // HOP_ANON_LINK_DAILY_CAP — global anonymous links per UTC day
	AnonLinkInterstitial bool          // HOP_ANON_LINK_INTERSTITIAL — browsers see a confirmation page before an anonymous redirect

	// Accounts (off unless OIDC_CLIENT_ID is set): sign in through an OIDC
	// provider, own your links/pastes, per-user API tokens, plan-based limits.
	OIDCIssuer        string   // OIDC_ISSUER (https://accounts.divyam.top)
	OIDCClientID      string   // OIDC_CLIENT_ID
	OIDCClientSecret  string   // OIDC_CLIENT_SECRET (optional; PKCE is always used)
	OIDCRedirectURLs  []string // OIDC_REDIRECT_URLS — comma list, one per host (…/auth/callback)
	SessionSecret     string   // HOP_SESSION_SECRET — encrypts the session cookie; random (and logged) if empty
	CookieDomain      string   // HOP_COOKIE_DOMAIN — e.g. divyam.top so one sign-in covers both hosts
	BillingURL        string   // BILLING_URL — internal entitlements service (http://billing:8084)
	BillingToken      string   // BILLING_INTERNAL_TOKEN
	BillingAccountURL string   // HOP_BILLING_ACCOUNT_URL — where users upgrade (linked from /account)
	// Admin sessions: a signed-in user whose roles claim contains one of AdminRoles
	// browses and deletes everything, like the owner token. OFF unless HOP_ADMIN_ROLES
	// is set — the claim comes from the IdP, so opting in is a decision to trust it.
	AdminRoles []string // HOP_ADMIN_ROLES — comma list, e.g. "admin"; empty disables admin sessions
	RolesClaim string   // HOP_ROLES_CLAIM — id_token claim holding a flat array of role names
	Plans      map[string]planLimits
}

func loadConfig() (Config, error) {
	c := Config{
		Listen:          env("HOP_LISTEN", ":8090"),
		DBPath:          env("HOP_DB", "/data/hop.db"),
		Token:           os.Getenv("HOP_TOKEN"),
		LinksHost:       strings.ToLower(env("HOP_LINKS_HOST", "go.divyam.top")),
		PasteHost:       strings.ToLower(env("HOP_PASTE_HOST", "paste.divyam.top")),
		MaxPasteBytes:   256 << 10,
		DefaultPasteTTL: 30 * 24 * time.Hour,
		RepoURL:         env("HOP_REPO_URL", "https://github.com/nihaldivyam/hop"),
		TrustProxy:      env("HOP_TRUST_PROXY", "true") == "true",
		PublicPastes:    env("HOP_PUBLIC_PASTES", "false") == "true",
		AnonMaxBytes:    32 << 10,
		AnonMaxTTL:      24 * time.Hour,
		AnonRateN:       5,
		AnonRatePer:     time.Hour,
		AnonBurst:       2,
		AnonDailyCap:    200,

		PublicLinks:          env("HOP_PUBLIC_LINKS", "false") == "true",
		AnonLinkMaxTTL:       7 * 24 * time.Hour,
		AnonLinkRateN:        5,
		AnonLinkRatePer:      time.Hour,
		AnonLinkBurst:        2,
		AnonLinkDailyCap:     200,
		AnonLinkInterstitial: env("HOP_ANON_LINK_INTERSTITIAL", "true") == "true",

		OIDCIssuer:        strings.TrimRight(os.Getenv("OIDC_ISSUER"), "/"),
		OIDCClientID:      os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:  os.Getenv("OIDC_CLIENT_SECRET"),
		RolesClaim:        env("HOP_ROLES_CLAIM", "roles"),
		SessionSecret:     os.Getenv("HOP_SESSION_SECRET"),
		CookieDomain:      os.Getenv("HOP_COOKIE_DOMAIN"),
		BillingURL:        os.Getenv("BILLING_URL"),
		BillingToken:      os.Getenv("BILLING_INTERNAL_TOKEN"),
		BillingAccountURL: env("HOP_BILLING_ACCOUNT_URL", "https://billing.divyam.top/account"),
		Plans:             defaultPlans(),
	}
	for _, u := range strings.Split(os.Getenv("OIDC_REDIRECT_URLS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.OIDCRedirectURLs = append(c.OIDCRedirectURLs, u)
		}
	}
	for _, role := range strings.Split(os.Getenv("HOP_ADMIN_ROLES"), ",") {
		if role = strings.TrimSpace(role); role != "" {
			c.AdminRoles = append(c.AdminRoles, role)
		}
	}
	if c.OIDCClientID != "" && c.OIDCIssuer == "" {
		return c, fmt.Errorf("OIDC_CLIENT_ID is set but OIDC_ISSUER is empty")
	}
	if v := os.Getenv("HOP_MAX_PASTE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("HOP_MAX_PASTE_BYTES: %q is not a positive integer", v)
		}
		c.MaxPasteBytes = n
	}
	if v := os.Getenv("HOP_DEFAULT_PASTE_TTL"); v != "" {
		d, err := parseTTL(v)
		if err != nil {
			return c, fmt.Errorf("HOP_DEFAULT_PASTE_TTL: %w", err)
		}
		c.DefaultPasteTTL = d
	}
	if v := os.Getenv("HOP_ANON_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("HOP_ANON_MAX_BYTES: %q is not a positive integer", v)
		}
		c.AnonMaxBytes = n
	}
	if v := os.Getenv("HOP_ANON_MAX_TTL"); v != "" {
		d, err := parseTTL(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("HOP_ANON_MAX_TTL: %q must be a positive duration", v)
		}
		c.AnonMaxTTL = d
	}
	if v := os.Getenv("HOP_ANON_RATE"); v != "" {
		n, per, err := parseRate(v)
		if err != nil {
			return c, fmt.Errorf("HOP_ANON_RATE: %w", err)
		}
		c.AnonRateN, c.AnonRatePer = n, per
	}
	if v := os.Getenv("HOP_ANON_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("HOP_ANON_BURST: %q is not a positive integer", v)
		}
		c.AnonBurst = n
	}
	if v := os.Getenv("HOP_ANON_DAILY_CAP"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return c, fmt.Errorf("HOP_ANON_DAILY_CAP: %q is not a non-negative integer", v)
		}
		c.AnonDailyCap = n
	}
	if v := os.Getenv("HOP_ANON_LINK_MAX_TTL"); v != "" {
		d, err := parseTTL(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("HOP_ANON_LINK_MAX_TTL: %q must be a positive duration", v)
		}
		c.AnonLinkMaxTTL = d
	}
	if v := os.Getenv("HOP_ANON_LINK_RATE"); v != "" {
		n, per, err := parseRate(v)
		if err != nil {
			return c, fmt.Errorf("HOP_ANON_LINK_RATE: %w", err)
		}
		c.AnonLinkRateN, c.AnonLinkRatePer = n, per
	}
	if v := os.Getenv("HOP_ANON_LINK_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("HOP_ANON_LINK_BURST: %q is not a positive integer", v)
		}
		c.AnonLinkBurst = n
	}
	if v := os.Getenv("HOP_ANON_LINK_DAILY_CAP"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return c, fmt.Errorf("HOP_ANON_LINK_DAILY_CAP: %q is not a non-negative integer", v)
		}
		c.AnonLinkDailyCap = n
	}
	return c, nil
}

// parseRate reads "N/period" ("5/1h", "20/10m") into N creates per period.
func parseRate(s string) (int, time.Duration, error) {
	parts := strings.SplitN(strings.TrimSpace(s), "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("want N/period (e.g. 5/1h), got %q", s)
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || n <= 0 {
		return 0, 0, fmt.Errorf("want N/period (e.g. 5/1h), got %q", s)
	}
	per, err := parseTTL(parts[1])
	if err != nil || per <= 0 {
		return 0, 0, fmt.Errorf("want N/period (e.g. 5/1h), got %q", s)
	}
	return n, per, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// parseTTL accepts Go durations ("90m", "36h") plus "d" and "w" suffixes
// ("7d", "2w"). "0" means no expiry. Negative values are rejected.
func parseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	var mult time.Duration
	switch {
	case strings.HasSuffix(s, "d"):
		mult = 24 * time.Hour
	case strings.HasSuffix(s, "w"):
		mult = 7 * 24 * time.Hour
	}
	if mult != 0 {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad ttl %q", s)
		}
		return time.Duration(n * float64(mult)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad ttl %q", s)
	}
	return d, nil
}
