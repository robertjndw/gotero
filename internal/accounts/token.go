package accounts

import "encoding/base64"

// URL-safe so tokens can sit in a query string without escaping.
func encodeToken(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
