// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

//go:build ignore

// mkfixtures regenerates the registry verification fixtures: a tiny fake
// provider zip, a SHA256SUMS listing it, detached signatures by two
// throwaway keys, and the public keys. Run from the repo root:
//
//	go run ./internal/registry/testdata/mkfixtures.go
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const dir = "internal/registry/testdata"

func main() {
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	mod := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, body := range map[string]string{
		"terraform-provider-fake_v1.0.0": "#!/bin/sh\necho not a real provider\n",
	} {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mod}
		h.SetMode(0o755)
		w := must(zw.CreateHeader(h))
		must(w.Write([]byte(body)))
	}
	h := &zip.FileHeader{Name: "LICENSE", Method: zip.Deflate, Modified: mod}
	h.SetMode(0o644)
	must(must(zw.CreateHeader(h)).Write([]byte("license text\n")))
	check(zw.Close())
	write("terraform-provider-fake_1.0.0_linux_amd64.zip", zbuf.Bytes())

	sum := sha256.Sum256(zbuf.Bytes())
	sums := fmt.Sprintf("%s  terraform-provider-fake_1.0.0_linux_amd64.zip\n%064d  terraform-provider-fake_1.0.0_darwin_arm64.zip\n",
		hex.EncodeToString(sum[:]), 0)
	write("SHA256SUMS", []byte(sums))

	cfg := &packet.Config{Time: func() time.Time { return mod }}
	for _, k := range []struct{ name, file, sig string }{
		{"provider-bridge test key", "key.asc", "SHA256SUMS.sig"},
		{"provider-bridge other key", "other-key.asc", "SHA256SUMS.other.sig"},
	} {
		e := must(openpgp.NewEntity(k.name, "throwaway", "test@invalid", cfg))
		var pub bytes.Buffer
		aw := must(armor.Encode(&pub, openpgp.PublicKeyType, nil))
		check(e.Serialize(aw))
		check(aw.Close())
		write(k.file, append(pub.Bytes(), '\n'))
		var sig bytes.Buffer
		check(openpgp.DetachSign(&sig, e, bytes.NewReader([]byte(sums)), cfg))
		write(k.sig, sig.Bytes())
	}
}

func write(name string, b []byte) { check(os.WriteFile(filepath.Join(dir, name), b, 0o644)) }

func must[T any](v T, err error) T {
	check(err)
	return v
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
