package main

import (
	"crypto/rand"
	"math/big"
	"regexp"
	"strings"
)

// base58: no 0/O/I/l, so slugs survive being read aloud or retyped.
const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func randomID(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(base58)))
	for i := range b {
		x, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err) // crypto/rand failing is not something to carry on from
		}
		b[i] = base58[x.Int64()]
	}
	return string(b)
}

var slugRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// reserved are path segments that can never be slugs or paste ids because the
// router owns them.
var reserved = map[string]bool{
	"api": true, "healthz": true, "static": true, "raw": true, "go": true,
	"favicon.ico": true, "robots.txt": true, "p": true, "admin": true,
	"sitemap.xml": true, "security.txt": true, "well-known": true, ".well-known": true,
	"login": true, "logout": true, "auth": true, "me": true, "account": true, "signup": true, "signin": true,
}

func validSlug(s string) bool { return slugRe.MatchString(s) && !reserved[s] }

// Custom paste ids ("name your own URL"): 1–15 chars, letters/digits/-/_,
// starting with a letter or digit. Random ids are 8-char base58 and can in
// principle collide with a valid name; uniqueness is enforced by the store either way.
var pasteIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,14}$`)

const pasteIDRule = "1-15 chars, letters/digits/-/_ (starting with a letter or digit), not a reserved name"

func validPasteID(s string) bool { return pasteIDRe.MatchString(s) && !reserved[strings.ToLower(s)] }
