package pgadmin

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SCRAM-SHA-256 (RFC 5802, RFC 7677) without channel binding. Passwords are
// used as given; libpq's SASLprep normalization only matters for non-ASCII
// passwords.

const (
	// gs2Header declares no channel binding; "biws" is its base64.
	gs2Header    = "n,,"
	scramNonceSz = 18
)

func newSCRAMNonce() string {
	b := make([]byte, scramNonceSz)
	_, _ = rand.Read(b) // never fails

	return base64.StdEncoding.EncodeToString(b)
}

// scramClientFirstBare is the client-first-message without its GS2 header.
// Postgres ignores the user here and takes the one from the startup.
func scramClientFirstBare(user, nonce string) string {
	return "n=" + user + ",r=" + nonce
}

// scramClientFinal answers serverFirst and returns the client-final-message
// with the server signature that server-final must carry.
func scramClientFinal(password, clientFirstBare, serverFirst string) (string, []byte, error) {
	attrs := scramAttributes(serverFirst)
	nonce, salt64, iter := attrs["r"], attrs["s"], attrs["i"]

	_, clientNonce, _ := strings.Cut(clientFirstBare, ",r=")
	if !strings.HasPrefix(nonce, clientNonce) || len(nonce) == len(clientNonce) {
		return "", nil, errors.New("pgadmin: SCRAM server nonce does not extend the client nonce")
	}
	salt, err := base64.StdEncoding.DecodeString(salt64)
	if err != nil || len(salt) == 0 {
		return "", nil, errors.New("pgadmin: SCRAM salt is invalid")
	}
	iterations, err := strconv.Atoi(iter)
	if err != nil || iterations < 1 {
		return "", nil, fmt.Errorf("pgadmin: SCRAM iteration count %q is invalid", iter)
	}

	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", nil, fmt.Errorf("pgadmin: SCRAM: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")

	withoutProof := "c=" + base64.StdEncoding.EncodeToString([]byte(gs2Header)) + ",r=" + nonce
	authMessage := clientFirstBare + "," + serverFirst + "," + withoutProof

	clientSignature := hmacSHA256(storedKey[:], authMessage)
	proof := make([]byte, len(clientKey))
	subtle.XORBytes(proof, clientKey, clientSignature)

	final := withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)

	return final, hmacSHA256(serverKey, authMessage), nil
}

// scramVerifyServerFinal checks that the server knew the password too.
func scramVerifyServerFinal(serverFinal string, serverSignature []byte) error {
	attrs := scramAttributes(serverFinal)
	if e, ok := attrs["e"]; ok {
		return fmt.Errorf("pgadmin: SCRAM server error: %s", e)
	}
	got, err := base64.StdEncoding.DecodeString(attrs["v"])
	if err != nil || !hmac.Equal(got, serverSignature) {
		return errors.New("pgadmin: SCRAM server signature does not match")
	}

	return nil
}

func scramAttributes(msg string) map[string]string {
	attrs := make(map[string]string)
	for part := range strings.SplitSeq(msg, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			attrs[k] = v
		}
	}

	return attrs
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))

	return m.Sum(nil)
}
