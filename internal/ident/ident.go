package ident

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const shortHashLen = 8

// ShortHash returns a short, stable hex digest of parts.
func ShortHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))

	return hex.EncodeToString(sum[:])[:shortHashLen]
}
