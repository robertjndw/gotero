// Package auth holds credential handling shared by the Zotero API and the
// admin API: password hashing, API key authentication and the permission
// rules for libraries.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters, following the RFC 9106 "second recommended option"
// scaled for a server that also has to stay responsive: ~19 MiB and two
// passes take tens of milliseconds. They are stored in every hash, so they
// can be raised later without invalidating existing passwords.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// MinPasswordLength is enforced on every password change.
const MinPasswordLength = 10

var ErrInvalidHash = errors.New("auth: invalid password hash")

// HashPassword returns an encoded argon2id hash in PHC string format.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches encoded. It runs in
// constant time with respect to the hash contents.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrInvalidHash
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, ErrInvalidHash
	}
	// Refuse absurd parameters so a tampered hash row cannot pin the CPU or
	// exhaust memory.
	if memory == 0 || memory > 1<<20 || time == 0 || time > 10 || threads == 0 {
		return false, ErrInvalidHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 64 {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when a username does not exist, so login
// timing does not reveal which usernames are registered.
var dummyHash, _ = HashPassword("gotero-timing-equalizer")

// VerifyNoUser burns the same time as a real VerifyPassword call.
func VerifyNoUser(password string) {
	_, _ = VerifyPassword(dummyHash, password)
}

// HashToken is how API keys, session IDs and invite tokens are stored. These
// are high-entropy random strings, so a plain SHA-256 is sufficient and keeps
// per-request lookups cheap.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
