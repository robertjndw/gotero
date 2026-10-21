package zotero

import (
	"crypto/rand"
	"encoding/hex"
)

const (
	// KeyAlphabet is the character set of object keys. It leaves out 0, 1 and O
	// so keys can't be confused with look-alike characters.
	KeyAlphabet = "23456789ABCDEFGHIJKLMNPQRSTUVWXYZ"
	// KeyLength is the length of an object key.
	KeyLength = 8
	// APIKeyLength is the length of an API key.
	APIKeyLength = 24

	apiKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// randomString draws n characters from alphabet using rejection sampling, so
// no character is more likely than another (a plain modulo would favor the
// first few).
func randomString(alphabet string, n int) string {
	limit := 256 - 256%len(alphabet)
	out := make([]byte, n)
	buf := make([]byte, n+n/2+4)
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand failing means the OS entropy source is broken;
			// there is no safe fallback for generating credentials.
			panic("zotero: crypto/rand: " + err.Error())
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out[i] = alphabet[int(b)%len(alphabet)]
			i++
			if i == n {
				break
			}
		}
	}
	return string(out)
}

// NewKey returns a random object key.
func NewKey() string { return randomString(KeyAlphabet, KeyLength) }

// ValidKey reports whether s is a well-formed object key.
func ValidKey(s string) bool {
	if len(s) != KeyLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isKeyChar(s[i]) {
			return false
		}
	}
	return true
}

func isKeyChar(c byte) bool {
	switch {
	case c >= '2' && c <= '9':
		return true
	case c >= 'A' && c <= 'Z':
		return c != 'O'
	}
	return false
}

// NewAPIKey returns a random 24 character alphanumeric API key.
func NewAPIKey() string { return randomString(apiKeyAlphabet, APIKeyLength) }

// NewToken returns n random lowercase hex characters, for write tokens and
// upload keys. A write token is NewToken(32).
func NewToken(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic("zotero: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)[:n]
}
