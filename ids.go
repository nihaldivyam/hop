package main

import (
	"crypto/rand"
	"math/big"
	"regexp"
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
	"api": true, "healthz": true, "static": true, "raw": true,
	"favicon.ico": true, "robots.txt": true, "p": true, "admin": true,
}

func validSlug(s string) bool { return slugRe.MatchString(s) && !reserved[s] }
