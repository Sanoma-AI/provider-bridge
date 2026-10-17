// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package registry

import (
	"embed"
	"fmt"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// keyFiles holds the armored public keys of keys/; keys/README.md records
// where each came from.
//
//go:embed keys/*.asc
var keyFiles embed.FS

// trustedFiles names the key files trusted for each provider
// ("namespace/type").
var trustedFiles = map[string][]string{
	"stripe/stripe":     {"keys/stripe_stripe.asc"},
	"supabase/supabase": {"keys/supabase_supabase.asc"},
}

// Keyring maps a provider, "namespace/type" in lower case, to signing keys
// trusted for its releases in addition to the keys the registry lists.
// A key here never makes an unsigned release acceptable: the SHA256SUMS
// signature must still verify against one of the keys.
type Keyring map[string][]SigningKey

// For returns the keys trusted for src, whatever registry host serves it.
func (k Keyring) For(src Source) []SigningKey {
	return k[strings.ToLower(src.Namespace+"/"+src.Type)]
}

// TrustedKeys returns the keyring shipped with the bridge (keys/).
var TrustedKeys = sync.OnceValue(func() Keyring {
	kr, err := loadKeyring(trustedFiles)
	if err != nil {
		panic(err) // the files are embedded; a test loads them all
	}
	return kr
})

func loadKeyring(files map[string][]string) (Keyring, error) {
	kr := Keyring{}
	for provider, names := range files {
		for _, name := range names {
			b, err := keyFiles.ReadFile(name)
			if err != nil {
				return nil, err
			}
			ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(string(b)))
			if err != nil {
				return nil, fmt.Errorf("trusted key %s: %w", name, err)
			}
			if len(ring) != 1 {
				return nil, fmt.Errorf("trusted key %s: holds %d keys, want one", name, len(ring))
			}
			kr[provider] = append(kr[provider], SigningKey{KeyID: ring[0].PrimaryKey.KeyIdString(), ASCIIArmor: string(b)})
		}
	}
	return kr, nil
}
