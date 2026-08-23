package main

import (
	"net/url"
	"reflect"
	"testing"
)

func TestRolesFrom(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		key    string
		want   []string
	}{
		{
			name:   "array of strings",
			claims: map[string]any{"roles": []any{"admin", "vip"}},
			key:    "roles", want: []string{"admin", "vip"},
		},
		{
			name:   "single string",
			claims: map[string]any{"roles": "admin"},
			key:    "roles", want: []string{"admin"},
		},
		{
			name:   "custom claim name",
			claims: map[string]any{"groups": []any{"vip"}},
			key:    "groups", want: []string{"vip"},
		},
		{
			name:   "missing claim",
			claims: map[string]any{"sub": "u1"},
			key:    "roles", want: nil,
		},
		{
			// Zitadel's native shape. Guessing at the keys of a map would hand out
			// admin on a claim we do not actually understand, so we take nothing.
			name: "nested map yields nothing",
			claims: map[string]any{"urn:zitadel:iam:org:project:roles": map[string]any{
				"admin": map[string]any{"orgid": "divyam.top"},
			}},
			key: "urn:zitadel:iam:org:project:roles", want: nil,
		},
		{
			name:   "non-string entries are skipped",
			claims: map[string]any{"roles": []any{"vip", 42, "", nil}},
			key:    "roles", want: []string{"vip"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rolesFrom(tc.claims, tc.key); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rolesFrom = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestHasAdminRole(t *testing.T) {
	t.Run("off when HOP_ADMIN_ROLES is empty", func(t *testing.T) {
		s := &Server{cfg: Config{}}
		if s.hasAdminRole([]string{"admin", "vip"}) {
			t.Fatal("admin granted with no configured admin roles")
		}
	})
	t.Run("matches a configured role", func(t *testing.T) {
		s := &Server{cfg: Config{AdminRoles: []string{"admin"}}}
		if !s.hasAdminRole([]string{"vip", "admin"}) {
			t.Fatal("configured role not matched")
		}
	})
	t.Run("does not match an unlisted role", func(t *testing.T) {
		s := &Server{cfg: Config{AdminRoles: []string{"admin"}}}
		if s.hasAdminRole([]string{"vip"}) {
			t.Fatal("unlisted role granted admin")
		}
	})
	t.Run("is case sensitive", func(t *testing.T) {
		s := &Server{cfg: Config{AdminRoles: []string{"admin"}}}
		if s.hasAdminRole([]string{"Admin"}) {
			t.Fatal("case-insensitive match would widen the gate silently")
		}
	})
}

// The important distinction: an admin READS and DELETES everything, but rows they
// CREATE are still stamped as theirs. Conflating the two would make an admin's own
// pastes ownerless and indistinguishable from anonymous ones.
func TestAdminScopes(t *testing.T) {
	owner := principal{Kind: "owner", Via: "token"}
	admin := principal{Kind: "user", Sub: "u-admin", Via: "session", Admin: true}
	user := principal{Kind: "user", Sub: "u-plain", Via: "session"}
	anon := principal{Kind: "anon"}

	for _, tc := range []struct {
		name                string
		p                   principal
		wantOwner, wantView string
		wantSeeAll          bool
	}{
		{"owner token", owner, "", "", true},
		{"admin session", admin, "u-admin", "", true},
		{"plain user", user, "u-plain", "u-plain", false},
		{"anonymous", anon, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.ownerScope(); got != tc.wantOwner {
				t.Errorf("ownerScope = %q, want %q", got, tc.wantOwner)
			}
			if got := tc.p.viewScope(); got != tc.wantView {
				t.Errorf("viewScope = %q, want %q", got, tc.wantView)
			}
			if got := tc.p.canSeeAll(); got != tc.wantSeeAll {
				t.Errorf("canSeeAll = %v, want %v", got, tc.wantSeeAll)
			}
		})
	}
}

// Admin is a property of a signed-in session only. A stray Admin=true on a
// non-user principal must not widen anything.
func TestAdminOnlyAppliesToSessions(t *testing.T) {
	stray := principal{Kind: "anon", Admin: true}
	if stray.isAdmin() {
		t.Fatal("anon principal reported as admin")
	}
	if stray.canSeeAll() {
		t.Fatal("anon principal could see everything")
	}
}

// A "Sign up" button that links straight at the provider's login page is not a
// sign-up flow: with no auth request in flight the provider has nothing to do and
// bounces the visitor to its default redirect, which reads as a dead button.
// /login?signup=1 starts a real authorization request carrying prompt=create.
func TestSignupStartsRegistrationFlow(t *testing.T) {
	e := accountsServer(t, nil)
	for _, tc := range []struct {
		name, query string
		wantPrompt  bool
	}{
		{"plain login", "", false},
		{"signup", "?signup=1", true},
		{"signup=0 is not signup", "?signup=0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := do(t, e.ts, "GET", "paste.example", "/login"+tc.query, "", "", nil, nil)
			if r.StatusCode != 302 {
				t.Fatalf("status = %d, want 302", r.StatusCode)
			}
			q, err := url.Parse(r.Header.Get("Location"))
			if err != nil {
				t.Fatalf("bad Location: %v", err)
			}
			if got := q.Query().Get("prompt") == "create"; got != tc.wantPrompt {
				t.Fatalf("prompt=create present = %v, want %v (Location: %s)", got, tc.wantPrompt, q)
			}
			// PKCE must survive whichever branch we took.
			if q.Query().Get("code_challenge") == "" {
				t.Errorf("PKCE challenge missing from %s", q)
			}
		})
	}
}
