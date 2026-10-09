// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func key(t *testing.T, name string) SigningKey {
	return SigningKey{KeyID: name, ASCIIArmor: string(readTestdata(t, name))}
}

func TestVerifySums(t *testing.T) {
	sums := readTestdata(t, "SHA256SUMS")
	sig := readTestdata(t, "SHA256SUMS.sig")
	otherSig := readTestdata(t, "SHA256SUMS.other.sig")

	t.Run("happy", func(t *testing.T) {
		s, err := VerifySums(sums, sig, []SigningKey{key(t, "key.asc")})
		if err != nil {
			t.Fatal(err)
		}
		if s.KeyID == "" || s.Expired {
			t.Fatalf("signer = %+v", s)
		}
	})
	t.Run("second key verifies", func(t *testing.T) {
		if _, err := VerifySums(sums, sig, []SigningKey{key(t, "other-key.asc"), key(t, "key.asc")}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("tampered document", func(t *testing.T) {
		bad := bytes.Replace(sums, []byte("linux_amd64"), []byte("linux_arm64"), 1)
		if _, err := VerifySums(bad, sig, []SigningKey{key(t, "key.asc")}); err == nil {
			t.Fatal("tampered SHA256SUMS verified")
		}
	})
	t.Run("wrong signature", func(t *testing.T) {
		bad := bytes.Clone(sig)
		bad[len(bad)-3] ^= 0xff
		if _, err := VerifySums(sums, bad, []SigningKey{key(t, "key.asc")}); err == nil {
			t.Fatal("corrupt signature verified")
		}
	})
	t.Run("unknown key", func(t *testing.T) {
		_, err := VerifySums(sums, otherSig, []SigningKey{key(t, "key.asc")})
		if err == nil || !strings.Contains(err.Error(), "not signed by any") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no keys", func(t *testing.T) {
		if _, err := VerifySums(sums, sig, nil); !errors.Is(err, ErrNoSigningKeys) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("garbage key", func(t *testing.T) {
		if _, err := VerifySums(sums, sig, []SigningKey{{KeyID: "x", ASCIIArmor: "nope"}}); err == nil {
			t.Fatal("garbage key accepted")
		}
	})
}

func TestSumFor(t *testing.T) {
	sums := readTestdata(t, "SHA256SUMS")
	zip := readTestdata(t, "terraform-provider-fake_1.0.0_linux_amd64.zip")
	want := sha256.Sum256(zip)
	got, err := SumFor(sums, "terraform-provider-fake_1.0.0_linux_amd64.zip")
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("SumFor = %q, %v", got, err)
	}
	if _, err := SumFor(sums, "terraform-provider-fake_1.0.0_windows_amd64.zip"); err == nil {
		t.Fatal("missing filename found")
	}
}

func TestParseSource(t *testing.T) {
	for in, want := range map[string]string{
		"integrations/github":                       "integrations/github",
		"registry.opentofu.org/hashicorp/null":      "hashicorp/null",
		"Example.COM/Acme/Thing":                    "example.com/acme/thing",
		"127.0.0.1:8443/acme/thing":                 "127.0.0.1:8443/acme/thing",
		"registry.opentofu.org/integrations/github": "integrations/github",
		// Parses; whether the host may be used is the Client's decision.
		"registry.terraform.io/stripe/stripe": "registry.terraform.io/stripe/stripe",
	} {
		src, err := ParseSource(in)
		if err != nil || src.String() != want {
			t.Errorf("ParseSource(%q) = %q, %v; want %q", in, src, err, want)
		}
	}
	for _, in := range []string{
		"", "github", "a/b/c/d", "../x", "ns/../x", "h@x/a/b",
		"../foo/bar", "./foo/bar", "./ns/x", "a..b/ns/x", ".a/ns/x", "a./ns/x", "-a/ns/x", "a b/ns/x", "a:b/ns/x", "a:123456/ns/x",
	} {
		if _, err := ParseSource(in); err == nil {
			t.Errorf("ParseSource(%q) succeeded", in)
		}
	}
}

func TestTrustedKeys(t *testing.T) {
	keys := TrustedKeys().For(Source{Host: DefaultHost, Namespace: "stripe", Type: "stripe"})
	if len(keys) != 1 || keys[0].KeyID != "A6C87736961A5EF6" {
		t.Fatalf("stripe/stripe trusted keys = %+v", keys)
	}
	// The key verifies the stripe/stripe 0.3.0 SHA256SUMS that
	// registry.opentofu.org points at (copied from the GitHub release), whose
	// sha256 is the pin in testdata/pins.json.
	sums := readTestdata(t, "stripe_0.3.0_SHA256SUMS")
	if got := sha256.Sum256(sums); hex.EncodeToString(got[:]) != "2e3ae569e1bee7c64ae7f10e6ceaaf5bfab4db7af38979bab28f9b7a35a260c5" {
		t.Fatalf("stripe SHA256SUMS sha256 = %x", got)
	}
	signer, err := VerifySums(sums, readTestdata(t, "stripe_0.3.0_SHA256SUMS.sig"), keys)
	if err != nil || signer.KeyID != "A6C87736961A5EF6" {
		t.Fatalf("stripe SHA256SUMS: signer %+v, %v", signer, err)
	}
	// Every embedded key file is listed for a provider.
	files, err := keyFiles.ReadDir("keys")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, names := range trustedFiles {
		for _, n := range names {
			listed[n] = true
		}
	}
	for _, f := range files {
		if !listed["keys/"+f.Name()] {
			t.Errorf("keys/%s is embedded but trusted for no provider", f.Name())
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, ok := range []string{"1.2.3", "0.3.0", "6.13.0", "1.0.0-beta.1"} {
		if err := CheckVersion(ok); err != nil {
			t.Errorf("CheckVersion(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1.2", "~> 1.2", ">= 1.0.0", "latest", "1.2.3/../x"} {
		if err := CheckVersion(bad); err == nil {
			t.Errorf("CheckVersion(%q) succeeded", bad)
		}
	}
}

// fakeRegistry serves the testdata release of example/fake 1.0.0 for
// linux_amd64 over TLS.
func fakeRegistry(t *testing.T, keys ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var zipHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/terraform.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"providers.v1":"/v1/providers/"}`))
	})
	mux.HandleFunc("/v1/providers/example/fake/1.0.0/download/linux/amd64", func(w http.ResponseWriter, _ *http.Request) {
		var pkg Package
		pkg.Protocols = []string{"6.0"}
		pkg.OS, pkg.Arch = "linux", "amd64"
		pkg.Filename = "terraform-provider-fake_1.0.0_linux_amd64.zip"
		pkg.DownloadURL = "/files/" + pkg.Filename
		pkg.SHASumsURL = "/files/SHA256SUMS"
		pkg.SHASumsSignatureURL = "/files/SHA256SUMS.sig"
		for _, k := range keys {
			pkg.SigningKeys.GPGPublicKeys = append(pkg.SigningKeys.GPGPublicKeys, key(t, k))
		}
		_ = json.NewEncoder(w).Encode(pkg)
	})
	files := http.StripPrefix("/files/", http.FileServer(http.Dir("testdata")))
	mux.Handle("/files/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			zipHits.Add(1)
		}
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/redirect/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/files/SHA256SUMS", http.StatusFound)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &zipHits
}

func host(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "https://") }

func newTestCache(t *testing.T, srv *httptest.Server) *Cache {
	return &Cache{
		Dir:    t.TempDir(),
		Client: &Client{HTTP: srv.Client(), AllowHosts: []string{host(srv)}},
		GOOS:   "linux", GOARCH: "amd64",
	}
}

func fakeSource(srv *httptest.Server) Source {
	return Source{Host: host(srv), Namespace: "example", Type: "fake"}
}

func TestInstall(t *testing.T) {
	srv, zipHits := fakeRegistry(t, "key.asc")
	cache := newTestCache(t, srv)
	src := fakeSource(srv)
	sums := sha256.Sum256(readTestdata(t, "SHA256SUMS"))
	pin := hex.EncodeToString(sums[:])
	ctx := context.Background()

	inst, err := cache.Install(ctx, src, "1.0.0", pin)
	if err != nil {
		t.Fatal(err)
	}
	if inst.BinaryName != "terraform-provider-fake_v1.0.0" || inst.SumsSHA256 != pin {
		t.Fatalf("installed = %+v", inst)
	}
	entries, _ := os.ReadDir(inst.Dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "install.json,terraform-provider-fake_v1.0.0" {
		t.Fatalf("cache dir holds %v; only the binary and install.json belong there", names)
	}
	fi, err := os.Stat(inst.Binary())
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("binary not executable: %v %v", fi, err)
	}

	// Second install hits the cache.
	if _, err := cache.Install(ctx, src, "1.0.0", ""); err != nil {
		t.Fatal(err)
	}
	if zipHits.Load() != 1 {
		t.Fatalf("zip downloaded %d times", zipHits.Load())
	}

	// A different pin is refused, without downloading again.
	var pinErr *PinMismatchError
	if _, err := cache.Install(ctx, src, "1.0.0", strings.Repeat("ab", 32)); !errors.As(err, &pinErr) {
		t.Fatalf("err = %v", err)
	}

	// A modified cached binary is detected.
	if err := os.WriteFile(inst.Binary(), []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Install(ctx, src, "1.0.0", pin); err == nil || !strings.Contains(err.Error(), "changed since install") {
		t.Fatalf("err = %v", err)
	}
}

func TestLockFileHonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := lockFile(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock2, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	unlock2()
}

func TestInstallRefusesUnknownSigner(t *testing.T) {
	srv, zipHits := fakeRegistry(t, "other-key.asc")
	cache := newTestCache(t, srv)
	src := fakeSource(srv)
	if _, err := cache.Install(context.Background(), src, "1.0.0", ""); err == nil {
		t.Fatal("installed a release signed by an unlisted key")
	}
	if zipHits.Load() != 0 {
		t.Fatal("zip downloaded before the signature check")
	}
}

func TestInstallRefusesUnsigned(t *testing.T) {
	srv, _ := fakeRegistry(t)
	cache := newTestCache(t, srv)
	if _, err := cache.Install(context.Background(), fakeSource(srv), "1.0.0", ""); !errors.Is(err, ErrNoSigningKeys) {
		t.Fatalf("err = %v", err)
	}
}

// A release the registry lists without keys installs when the bridge
// trusts the signing key for that provider, and only then.
func TestInstallTrustedKey(t *testing.T) {
	srv, _ := fakeRegistry(t)
	ctx := context.Background()

	cache := newTestCache(t, srv)
	cache.TrustedKeys = Keyring{"example/other": {key(t, "key.asc")}, "example/fake": {key(t, "other-key.asc")}}
	if _, err := cache.Install(ctx, fakeSource(srv), "1.0.0", ""); err == nil || !strings.Contains(err.Error(), "not signed by any") {
		t.Fatalf("installed with only an unrelated trusted key: %v", err)
	}

	cache = newTestCache(t, srv)
	cache.TrustedKeys = Keyring{"example/fake": {key(t, "key.asc")}}
	inst, err := cache.Install(ctx, fakeSource(srv), "1.0.0", "")
	if err != nil {
		t.Fatal(err)
	}
	if inst.SignedBy == "" {
		t.Fatalf("installed = %+v", inst)
	}
}

func TestHostPolicy(t *testing.T) {
	srv, _ := fakeRegistry(t, "key.asc")
	cache := newTestCache(t, srv)
	cache.Client = &Client{HTTP: srv.Client()}
	var hostErr *HostNotAllowedError
	if _, err := cache.Install(context.Background(), fakeSource(srv), "1.0.0", ""); !errors.As(err, &hostErr) {
		t.Fatalf("err = %v", err)
	}
	if err := (&Client{}).checkAllowed(DefaultHost); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSOnly(t *testing.T) {
	srv, _ := fakeRegistry(t, "key.asc")
	c := &Client{HTTP: srv.Client(), AllowHosts: []string{host(srv)}}
	ctx := context.Background()
	if _, err := c.getBytes(ctx, srv.URL+"/redirect/", maxSumsBytes); err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("redirect to http: %v", err)
	}
	base, _ := url.Parse(srv.URL + "/v1/providers/")
	for _, ref := range []string{"http://example.com/x.zip", "ftp://example.com/x", "file:///etc/passwd"} {
		if _, err := resolveHTTPS(base, ref); err == nil {
			t.Errorf("resolveHTTPS(%q) succeeded", ref)
		}
	}
	if u, err := resolveHTTPS(base, "/files/x.zip"); err != nil || u.Scheme != "https" {
		t.Fatalf("relative URL: %v %v", u, err)
	}
}
