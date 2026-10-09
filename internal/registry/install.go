// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Installed describes a verified provider binary in the cache.
type Installed struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	// Protocols are the plugin protocol versions the registry lists, e.g. ["5.0"].
	Protocols []string `json:"protocols"`
	// SumsSHA256 is the hex sha256 of the release's SHA256SUMS document:
	// the pin that covers every platform.
	SumsSHA256 string `json:"sumsSha256"`
	// ZipSHA256 is the verified hex sha256 of the downloaded zip.
	ZipSHA256 string `json:"zipSha256"`
	// BinarySHA256 is the hex sha256 of the extracted binary, rechecked on
	// every Install.
	BinarySHA256 string `json:"binarySha256"`
	SignedBy     string `json:"signedBy"`
	SignerKeyExp bool   `json:"signerKeyExpired,omitempty"`
	// BinaryName is the file name of the binary inside Dir.
	BinaryName  string    `json:"binaryName"`
	InstalledAt time.Time `json:"installedAt"`

	// Dir is the cache directory of this release and platform.
	Dir string `json:"-"`
}

// Binary returns the absolute path of the provider executable.
func (i *Installed) Binary() string { return filepath.Join(i.Dir, i.BinaryName) }

// Cache installs verified provider releases under Dir.
type Cache struct {
	Dir    string
	Client *Client
	Logger *slog.Logger
	// GOOS and GOARCH default to the running platform.
	GOOS, GOARCH string
}

func (c *Cache) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *Cache) platform() (string, string) {
	goos, goarch := c.GOOS, c.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

// PinMismatchError reports a release whose SHA256SUMS differs from the pin.
type PinMismatchError struct {
	Source, Version, Want, Got string
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("%s %s: SHA256SUMS sha256 is %s, pinned %s", e.Source, e.Version, e.Got, e.Want)
}

// Install returns the cached release of src at version, downloading and
// verifying it first when absent. When pin is non-empty it must equal the
// hex sha256 of the release's SHA256SUMS document.
func (c *Cache) Install(ctx context.Context, src Source, version, pin string) (*Installed, error) {
	if err := CheckVersion(version); err != nil {
		return nil, err
	}
	pin = strings.ToLower(pin)
	goos, goarch := c.platform()
	dir := filepath.Join(c.Dir, src.Host, src.Namespace, src.Type, version, goos+"_"+goarch)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	unlock, err := lockFile(dir + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()

	inst, err := readInstalled(dir)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		inst, err = c.download(ctx, src, version, goos, goarch, dir)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%s %s: corrupt cache entry %s: %w", src, version, dir, err)
	}
	if pin != "" && inst.SumsSHA256 != pin {
		return nil, &PinMismatchError{Source: src.String(), Version: version, Want: pin, Got: inst.SumsSHA256}
	}
	got, err := FileSHA256(inst.Binary())
	if err != nil {
		return nil, err
	}
	if got != inst.BinarySHA256 {
		return nil, fmt.Errorf("%s %s: cached binary %s changed since install (sha256 %s, recorded %s)",
			src, version, inst.Binary(), got, inst.BinarySHA256)
	}
	return inst, nil
}

func readInstalled(dir string) (*Installed, error) {
	b, err := os.ReadFile(filepath.Join(dir, "install.json"))
	if err != nil {
		return nil, err
	}
	var inst Installed
	if err := json.Unmarshal(b, &inst); err != nil {
		return nil, err
	}
	if inst.BinaryName == "" || filepath.Base(inst.BinaryName) != inst.BinaryName {
		return nil, errors.New("install.json: invalid binaryName")
	}
	inst.Dir = dir
	return &inst, nil
}

func (c *Cache) download(ctx context.Context, src Source, version, goos, goarch, dir string) (*Installed, error) {
	log := c.logger().With("provider", src.String(), "version", version, "platform", goos+"_"+goarch)
	pkg, err := c.Client.Package(ctx, src, version, goos, goarch)
	if err != nil {
		return nil, err
	}
	sums, err := c.Client.getBytes(ctx, pkg.SHASumsURL, maxSumsBytes)
	if err != nil {
		return nil, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	sig, err := c.Client.getBytes(ctx, pkg.SHASumsSignatureURL, maxSumsBytes)
	if err != nil {
		return nil, fmt.Errorf("download SHA256SUMS signature: %w", err)
	}
	signer, err := VerifySums(sums, sig, pkg.SigningKeys.GPGPublicKeys)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", src, version, err)
	}
	if signer.Expired {
		log.Warn("SHA256SUMS signed by an expired key", "key_id", signer.KeyID)
	}
	want, err := SumFor(sums, pkg.Filename)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", src, version, err)
	}
	if pkg.SHASum != "" && !strings.EqualFold(pkg.SHASum, want) {
		return nil, fmt.Errorf("%s %s: registry shasum %s disagrees with SHA256SUMS %s", src, version, pkg.SHASum, want)
	}

	tmp, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".tmp-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	zipPath := filepath.Join(tmp, "package.zip")
	got, err := c.downloadFile(ctx, pkg.DownloadURL, zipPath)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", pkg.Filename, err)
	}
	if got != want {
		return nil, fmt.Errorf("%s %s: %s has sha256 %s, SHA256SUMS lists %s", src, version, pkg.Filename, got, want)
	}

	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		return nil, err
	}
	binName, binSum, err := extractProvider(zipPath, src.Type, binDir)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", src, version, err)
	}
	sumsSum := sha256.Sum256(sums)
	inst := &Installed{
		Source:       src.String(),
		Version:      version,
		OS:           goos,
		Arch:         goarch,
		Protocols:    pkg.Protocols,
		SumsSHA256:   hex.EncodeToString(sumsSum[:]),
		ZipSHA256:    got,
		BinarySHA256: binSum,
		SignedBy:     signer.KeyID,
		SignerKeyExp: signer.Expired,
		BinaryName:   binName,
		InstalledAt:  time.Now().UTC(),
	}
	b, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(binDir, "install.json"), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	// A stale directory without install.json (an interrupted older layout)
	// is replaced; the lock guarantees nobody else is writing it.
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.Rename(binDir, dir); err != nil {
		return nil, err
	}
	inst.Dir = dir
	log.Info("provider installed", "sums_sha256", inst.SumsSHA256, "signed_by", signer.KeyID, "dir", dir)
	return inst, nil
}

func (c *Cache) downloadFile(ctx context.Context, rawURL, path string) (string, error) {
	resp, err := c.Client.get(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxZipBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n > maxZipBytes {
		return "", fmt.Errorf("larger than %d bytes", maxZipBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractProvider copies the single terraform-provider-<type>* executable at
// the root of the zip into dir. Every other entry (LICENSE, README) is
// ignored, so nothing else from the archive lands on disk.
func extractProvider(zipPath, typ, dir string) (name, sum string, err error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = zr.Close() }()
	prefix := "terraform-provider-" + typ
	var found *zip.File
	for _, f := range zr.File {
		if strings.Contains(f.Name, "/") || strings.Contains(f.Name, `\`) || !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		if !f.Mode().IsRegular() {
			return "", "", fmt.Errorf("zip entry %s is not a regular file", f.Name)
		}
		if found != nil {
			return "", "", fmt.Errorf("zip has more than one %s* entry", prefix)
		}
		found = f
	}
	if found == nil {
		return "", "", fmt.Errorf("zip has no %s* executable", prefix)
	}
	if found.UncompressedSize64 > maxZipBytes {
		return "", "", fmt.Errorf("zip entry %s is larger than %d bytes", found.Name, maxZipBytes)
	}
	rc, err := found.Open()
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rc.Close() }()
	out, err := os.OpenFile(filepath.Join(dir, found.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(rc, maxZipBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", err
	}
	if n > maxZipBytes {
		return "", "", fmt.Errorf("zip entry %s is larger than %d bytes", found.Name, maxZipBytes)
	}
	return found.Name, hex.EncodeToString(h.Sum(nil)), nil
}

// FileSHA256 returns the hex sha256 of the file at path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
