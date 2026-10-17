// Package auth issues and checks the server's two credentials.
//
// A session token proves ownership of one session. It is generated when the
// session is created, returned once, and stored only as a SHA-256 hash: the
// token carries 256 bits of entropy, so a fast hash is enough and a database
// leak reveals nothing usable. Every write to a session must present it.
//
// An access key, when the operator configures any, gates every write on the
// server, so only people holding a key can broadcast through it.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

// Prefixes make the two credential kinds recognizable in logs and secret
// scanners.
const (
	TokenPrefix = "bgs_"
	KeyPrefix   = "bgk_"
)

// NewToken returns a fresh session token and the hash to store for it.
func NewToken() (token string, hash []byte) {
	token = TokenPrefix + random()
	return token, HashToken(token)
}

// HashToken is the stored form of a session token.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// CheckToken reports whether token matches the stored hash, in constant
// time. An empty hash never matches.
func CheckToken(token string, hash []byte) bool {
	if len(hash) != sha256.Size || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare(HashToken(token), hash) == 1
}

// NewKey returns a fresh access key.
func NewKey() string { return KeyPrefix + random() }

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails; see crypto/rand.Read
	return base64.RawURLEncoding.EncodeToString(b)
}

// Keys is the set of configured access keys.
type Keys struct {
	hashes [][sha256.Size]byte
}

// NewKeys holds keys for checking. An empty set disables the requirement.
func NewKeys(keys []string) *Keys {
	k := &Keys{}
	for _, key := range keys {
		k.hashes = append(k.hashes, sha256.Sum256([]byte(key)))
	}
	return k
}

// Required reports whether writes need an access key.
func (k *Keys) Required() bool { return len(k.hashes) > 0 }

// Valid reports whether key is one of the configured keys. Every key is
// compared, in constant time, so the timing reveals neither which key
// matched nor how much of one did.
func (k *Keys) Valid(key string) bool {
	if key == "" {
		return false
	}
	sum := sha256.Sum256([]byte(key))
	match := 0
	for i := range k.hashes {
		match |= subtle.ConstantTimeCompare(sum[:], k.hashes[i][:])
	}
	return match == 1
}

// KeyHeader carries the access key.
const KeyHeader = "X-Biga-Key"

// BearerToken extracts the token from an "Authorization: Bearer" header.
func BearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
