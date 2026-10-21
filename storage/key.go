package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// StagingDir is the reserved top-level name backends use for in-progress
// uploads. Callers can never use it as (the start of) a key.
const StagingDir = ".staging"

// ErrInvalidKey is returned (wrapped) when a key fails ValidateKey.
var ErrInvalidKey = errors.New("storage: invalid key")

// ValidateKey checks that key is safe to use with every bundled backend.
//
// Keys must be non-empty, relative, slash separated, free of empty, "." and
// ".." segments, backslashes and NUL bytes, and must not start with
// StagingDir. The rules are deliberately strict so a key can be mapped onto a
// filesystem path or a URL path without any escaping surprises.
func ValidateKey(key string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrInvalidKey, why) }
	switch {
	case key == "":
		return bad("empty key")
	case strings.HasPrefix(key, "/"):
		return bad("absolute key")
	case strings.ContainsAny(key, "\\\x00"):
		return bad("backslash or NUL in key")
	case strings.HasPrefix(key, StagingDir):
		return bad("reserved " + StagingDir + " prefix")
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "":
			return bad("empty path segment")
		case ".", "..":
			return bad("dot segment")
		}
	}
	return nil
}

// ValidatePrefix is like ValidateKey but for Walk prefixes: the empty prefix
// is allowed, and a trailing slash is fine. A prefix may also end mid-segment
// ("ab/abc"), so only the dangerous parts are rejected.
func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	p := strings.TrimSuffix(prefix, "/")
	if p == "" {
		return fmt.Errorf("%w: absolute prefix", ErrInvalidKey)
	}
	return ValidateKey(p)
}

// NewStagingName returns a random, collision-free name for a staging object.
func NewStagingName() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("storage: random staging name: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
