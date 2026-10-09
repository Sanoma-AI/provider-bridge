// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
)

// Signer describes the key that signed a SHA256SUMS document.
type Signer struct {
	KeyID string
	// Expired is true when the signature only verified with an expired key
	// or signature. OpenTofu accepts these with a warning (signatures made
	// before expiry stay meaningful); so does the bridge.
	Expired bool
}

// ErrNoSigningKeys is returned for releases the registry lists without keys.
var ErrNoSigningKeys = errors.New("release has no signing keys")

// VerifySums checks sig, a detached binary OpenPGP signature over sums,
// against each of keys in turn, in the way OpenTofu's findSigningKey does
// (internal/getproviders/package_authentication.go): unknown issuers are
// skipped, an expired key is remembered and accepted only if no other key
// verifies, and any other failure is fatal.
func VerifySums(sums, sig []byte, keys []SigningKey) (Signer, error) {
	if len(keys) == 0 {
		return Signer{}, ErrNoSigningKeys
	}
	var expired *Signer
	for _, key := range keys {
		keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(key.ASCIIArmor))
		if err != nil {
			return Signer{}, fmt.Errorf("decode signing key %s: %w", key.KeyID, err)
		}
		entity, err := openpgp.CheckDetachedSignature(keyring, bytes.NewReader(sums), bytes.NewReader(sig), nil)
		switch {
		case err == nil:
			return Signer{KeyID: keyID(entity, key)}, nil
		case errors.Is(err, pgperrors.ErrUnknownIssuer):
			continue
		case errors.Is(err, pgperrors.ErrKeyExpired), errors.Is(err, pgperrors.ErrSignatureExpired):
			if expired == nil {
				expired = &Signer{KeyID: keyID(entity, key), Expired: true}
			}
		default:
			return Signer{}, fmt.Errorf("check SHA256SUMS signature: %w", err)
		}
	}
	if expired != nil {
		return *expired, nil
	}
	return Signer{}, errors.New("check SHA256SUMS signature: not signed by any of the release's signing keys")
}

func keyID(entity *openpgp.Entity, key SigningKey) string {
	if entity != nil && entity.PrimaryKey != nil {
		return entity.PrimaryKey.KeyIdString()
	}
	return key.KeyID
}

// SumFor returns the hex sha256 that a SHA256SUMS document lists for
// filename.
func SumFor(sums []byte, filename string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		// sha256sum's binary mode prefixes the name with "*".
		if strings.TrimPrefix(fields[1], "*") != filename {
			continue
		}
		sum := strings.ToLower(fields[0])
		if b, err := hex.DecodeString(sum); err != nil || len(b) != 32 {
			return "", fmt.Errorf("SHA256SUMS: malformed hash for %s", filename)
		}
		return sum, nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("SHA256SUMS does not list %s", filename)
}
