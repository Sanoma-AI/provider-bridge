// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// DefaultHost is the registry used when a source address has no host.
const DefaultHost = "registry.opentofu.org"

// Size limits for untrusted downloads.
const (
	maxJSONBytes = 4 << 20
	maxSumsBytes = 1 << 20
	maxZipBytes  = 1 << 30
)

// Source is a provider source address such as "integrations/github".
type Source struct {
	Host      string
	Namespace string
	Type      string
}

var (
	namePart = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// hostname is a DNS name (letters, digits, inner hyphens per label) with
	// an optional port; it never contains "/", "..", "@" or a leading ".".
	hostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[0-9]{1,5})?$`)
)

// ParseSource parses "namespace/type" or "host/namespace/type". Every part
// is lower-cased, as OpenTofu normalizes them. Parsing does not decide
// whether a host may be used: Client does (see Client.AllowHosts).
func ParseSource(s string) (Source, error) {
	parts := strings.Split(strings.ToLower(s), "/")
	var src Source
	switch len(parts) {
	case 2:
		src = Source{Host: DefaultHost, Namespace: parts[0], Type: parts[1]}
	case 3:
		src = Source{Host: parts[0], Namespace: parts[1], Type: parts[2]}
	default:
		return Source{}, fmt.Errorf("invalid provider source %q: want namespace/type or host/namespace/type", s)
	}
	if !namePart.MatchString(src.Namespace) || !namePart.MatchString(src.Type) {
		return Source{}, fmt.Errorf("invalid provider source %q", s)
	}
	if err := CheckHost(src.Host); err != nil {
		return Source{}, err
	}
	return src, nil
}

// CheckHost rejects anything but a lower-case hostname with an optional
// port.
func CheckHost(host string) error {
	if len(host) > 253 || !hostname.MatchString(host) {
		return fmt.Errorf("invalid registry host %q", host)
	}
	return nil
}

// String returns the short form when the host is the default registry.
func (s Source) String() string {
	if s.Host == DefaultHost {
		return s.Namespace + "/" + s.Type
	}
	return s.Host + "/" + s.Namespace + "/" + s.Type
}

var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// CheckVersion rejects anything but an exact semantic version.
func CheckVersion(v string) error {
	if !versionRe.MatchString(v) {
		return fmt.Errorf("invalid provider version %q: want an exact version such as 1.2.3", v)
	}
	return nil
}

// Package is the registry's download description of one release for one
// platform.
type Package struct {
	Protocols           []string `json:"protocols"`
	OS                  string   `json:"os"`
	Arch                string   `json:"arch"`
	Filename            string   `json:"filename"`
	DownloadURL         string   `json:"download_url"`
	SHASumsURL          string   `json:"shasums_url"`
	SHASumsSignatureURL string   `json:"shasums_signature_url"`
	SHASum              string   `json:"shasum"`
	SigningKeys         struct {
		GPGPublicKeys []SigningKey `json:"gpg_public_keys"`
	} `json:"signing_keys"`
}

// SigningKey is one armored OpenPGP public key listed for a release.
type SigningKey struct {
	KeyID      string `json:"key_id"`
	ASCIIArmor string `json:"ascii_armor"`
}

// Client talks to provider registries over HTTPS. It only talks to
// DefaultHost and the hosts in AllowHosts: registries have their own terms
// of use, and one registry means one verification story.
type Client struct {
	// HTTP is the client used for every request; nil means a default
	// client. Either way redirects to anything but https are refused.
	HTTP *http.Client
	// AllowHosts are the registry hosts allowed besides DefaultHost.
	AllowHosts []string

	httpOnce  sync.Once
	hc        *http.Client
	mu        sync.Mutex
	discovery map[string]*url.URL
}

// HostNotAllowedError reports a source on a registry host the client is
// not allowed to use.
type HostNotAllowedError struct{ Host string }

func (e *HostNotAllowedError) Error() string {
	return fmt.Sprintf("registry host %s is not allowed (only %s, unless the bridge runs with --allow-host %s)", e.Host, DefaultHost, e.Host)
}

func (c *Client) checkAllowed(host string) error {
	if host == DefaultHost || slices.Contains(c.AllowHosts, host) {
		return nil
	}
	return &HostNotAllowedError{Host: host}
}

func (c *Client) http() *http.Client {
	c.httpOnce.Do(func() {
		var hc http.Client
		if c.HTTP != nil {
			hc = *c.HTTP
		}
		hc.CheckRedirect = httpsRedirectsOnly
		c.hc = &hc
	})
	return c.hc
}

func httpsRedirectsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to %s: not https", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// resolveHTTPS resolves ref against base and refuses anything but https.
func resolveHTTPS(base *url.URL, ref string) (*url.URL, error) {
	u, err := base.Parse(ref)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("refusing %s: not https", u.Redacted())
	}
	return u, nil
}

// providersBase resolves the host's providers.v1 endpoint via service
// discovery (/.well-known/terraform.json).
func (c *Client) providersBase(ctx context.Context, host string) (*url.URL, error) {
	if err := c.checkAllowed(host); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if u, ok := c.discovery[host]; ok {
		c.mu.Unlock()
		return u, nil
	}
	c.mu.Unlock()

	wellKnown := &url.URL{Scheme: "https", Host: host, Path: "/.well-known/terraform.json"}
	var doc struct {
		ProvidersV1 string `json:"providers.v1"`
	}
	if err := c.getJSON(ctx, wellKnown.String(), &doc); err != nil {
		return nil, fmt.Errorf("service discovery for %s: %w", host, err)
	}
	if doc.ProvidersV1 == "" {
		return nil, fmt.Errorf("service discovery for %s: host does not serve providers.v1", host)
	}
	base, err := resolveHTTPS(wellKnown, doc.ProvidersV1)
	if err != nil {
		return nil, fmt.Errorf("service discovery for %s: %w", host, err)
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	c.mu.Lock()
	if c.discovery == nil {
		c.discovery = map[string]*url.URL{}
	}
	c.discovery[host] = base
	c.mu.Unlock()
	return base, nil
}

// Package fetches the download description of src at version for one
// platform. Relative URLs in the answer are resolved against the request URL.
func (c *Client) Package(ctx context.Context, src Source, version, goos, goarch string) (*Package, error) {
	if err := CheckVersion(version); err != nil {
		return nil, err
	}
	base, err := c.providersBase(ctx, src.Host)
	if err != nil {
		return nil, err
	}
	u, err := base.Parse(strings.Join([]string{
		url.PathEscape(src.Namespace), url.PathEscape(src.Type), url.PathEscape(version),
		"download", url.PathEscape(goos), url.PathEscape(goarch),
	}, "/"))
	if err != nil {
		return nil, err
	}
	var pkg Package
	if err := c.getJSON(ctx, u.String(), &pkg); err != nil {
		return nil, fmt.Errorf("download info for %s %s %s_%s: %w", src, version, goos, goarch, err)
	}
	for _, field := range []*string{&pkg.DownloadURL, &pkg.SHASumsURL, &pkg.SHASumsSignatureURL} {
		if *field == "" {
			return nil, fmt.Errorf("download info for %s %s: missing URL", src, version)
		}
		abs, err := resolveHTTPS(u, *field)
		if err != nil {
			return nil, fmt.Errorf("download info for %s %s: %w", src, version, err)
		}
		*field = abs.String()
	}
	if pkg.Filename == "" || strings.ContainsAny(pkg.Filename, `/\`) {
		return nil, fmt.Errorf("download info for %s %s: invalid filename %q", src, version, pkg.Filename)
	}
	return &pkg, nil
}

func (c *Client) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "provider-bridge")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	return resp, nil
}

func (c *Client) getBytes(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	resp, err := c.get(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", rawURL, limit)
	}
	return b, nil
}

func (c *Client) getJSON(ctx context.Context, rawURL string, v any) error {
	b, err := c.getBytes(ctx, rawURL, maxJSONBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("GET %s: %w", rawURL, err)
	}
	return nil
}
