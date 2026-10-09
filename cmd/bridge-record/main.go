// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

// Command bridge-record records provider schemas and Import/Read replies
// into testdata/, scrubbed of secrets, so the bridge and its clients can be
// tested without credentials. It never runs in CI.
//
//	bridge-record --provider integrations/github --version 6.13.0 \
//	  --config '{"owner":"Sanoma-AI"}' --config-env token=GITHUB_TOKEN \
//	  --import github_repository=provider-bridge \
//	  --import github_branch_protection=provider-bridge:main
//
//	bridge-record --provider registry.terraform.io/stripe/stripe --version 0.3.0 --schema-only
//
// It writes testdata/schemas/<ns>_<type>_<version>.json and
// testdata/replies/<ns>_<type>_<version>/<resource type>/<id>/{import,read}.json.
// Each reply fixture is {provider, request, response, scrubbed}: request
// and response are the bridge API messages in Connect's JSON form; a failed
// call has response {"error": {code, message, diagnostics}}.
//
// Scrubbing: every attribute the schema marks sensitive and every --scrub
// <type>.<path> is replaced by "<scrubbed>" (strings inside it; the type is
// kept), every --config-env secret is replaced wherever it appears, and the
// command refuses to write a file in which any secret survives.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	bridgev1 "github.com/Sanoma-AI/provider-bridge/gen/bridge/v1"
	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/registry"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/server"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type options struct {
	source, version, pin string
	config               string
	configEnv            multi
	imports              multi
	scrub                multi
	out                  string
	cacheDir             string
	schemaOnly           bool
	tfLog                string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bridge-record:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var o options
	fs := flag.NewFlagSet("bridge-record", flag.ContinueOnError)
	fs.StringVar(&o.source, "provider", "", "provider source address (required)")
	fs.StringVar(&o.version, "version", "", "exact provider version (required)")
	fs.StringVar(&o.pin, "sha256", "", "pin: sha256 of the release's SHA256SUMS (default: from <out>/pins.json)")
	fs.StringVar(&o.config, "config", "{}", "provider config JSON without secrets")
	fs.Var(&o.configEnv, "config-env", "ATTR=ENV: set config attribute ATTR from environment variable ENV and scrub its value (repeatable)")
	fs.Var(&o.imports, "import", "TYPE=ID: import and read one resource (repeatable)")
	fs.Var(&o.scrub, "scrub", "TYPE.PATH: also scrub this attribute path (repeatable)")
	fs.StringVar(&o.out, "out", "testdata", "testdata directory")
	fs.StringVar(&o.cacheDir, "cache-dir", defaultCacheDir(), "provider cache directory")
	fs.BoolVar(&o.schemaOnly, "schema-only", false, "record the schema only (needs no credentials)")
	fs.StringVar(&o.tfLog, "tf-log", "", "TF_LOG level for the provider")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.source == "" || o.version == "" || fs.NArg() > 0 {
		return errors.New("--provider and --version are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return record(ctx, &o)
}

func defaultCacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "provider-bridge-test")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "provider-bridge-test")
	}
	return ".provider-bridge-cache"
}

type recorder struct {
	o        *options
	svc      *server.Service
	ref      *bridgev1.ProviderRef
	info     providerInfo
	schema   *schema.ProviderSchema
	secrets  map[string]string // env name -> value
	scrubber *scrubber
	dir      string
}

type providerInfo struct {
	Source   string `json:"source"`
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
	Protocol int32  `json:"protocol"`
}

type fixture struct {
	Provider providerInfo    `json:"provider"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
	Scrubbed []string        `json:"scrubbed"`
}

func record(ctx context.Context, o *options) error {
	src, err := registry.ParseSource(o.source)
	if err != nil {
		return err
	}
	if o.pin == "" {
		o.pin = pinFromFile(filepath.Join(o.out, "pins.json"), src.String(), o.version)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	launcher := &provider.RegistryLauncher{
		Cache:  &registry.Cache{Dir: o.cacheDir, Client: &registry.Client{}, Logger: log},
		Logger: log,
		TFLog:  o.tfLog,
	}
	m := provider.NewManager(launcher, log)
	defer m.Shutdown()

	r := &recorder{
		o:        o,
		svc:      server.NewService(m),
		ref:      &bridgev1.ProviderRef{Source: src.String(), Version: o.version, Sha256: o.pin},
		secrets:  map[string]string{},
		scrubber: newScrubber(o.scrub),
	}
	name := src.Namespace + "_" + src.Type + "_" + o.version
	r.dir = filepath.Join(o.out, "replies", name)

	sres, err := r.svc.GetSchema(ctx, connect.NewRequest(&bridgev1.GetSchemaRequest{Provider: r.ref}))
	if err != nil {
		return fmt.Errorf("GetSchema: %w", err)
	}
	r.ref.Sha256 = sres.Msg.GetSha256()
	r.info = providerInfo{Source: src.String(), Version: o.version, SHA256: r.ref.Sha256, Protocol: sres.Msg.GetProtocol()}
	if r.schema, err = schema.Unmarshal([]byte(sres.Msg.GetSchemaJson())); err != nil {
		return err
	}
	schemaPath := filepath.Join(o.out, "schemas", name+".json")
	if err := writeFile(schemaPath, []byte(sres.Msg.GetSchemaJson())); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d resource types, %d bytes)\n", schemaPath, len(r.schema.Resources), len(sres.Msg.GetSchemaJson()))
	if o.schemaOnly || len(o.imports) == 0 {
		return nil
	}

	cfg, err := r.config()
	if err != nil {
		return err
	}
	if _, err := r.svc.Configure(ctx, connect.NewRequest(&bridgev1.ConfigureRequest{Provider: r.ref, ConfigJson: cfg})); err != nil {
		return fmt.Errorf("Configure: %w", r.scrubErr(err))
	}
	for _, imp := range o.imports {
		typeName, id, ok := strings.Cut(imp, "=")
		if !ok || typeName == "" || id == "" {
			return fmt.Errorf("--import %q: want TYPE=ID", imp)
		}
		if err := r.importAndRead(ctx, typeName, id); err != nil {
			return err
		}
	}
	return nil
}

// pinFromFile returns the pin testdata/pins.json records for source at
// version, or "".
func pinFromFile(path, source, version string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pins map[string]struct{ Version, SHA256 string }
	if json.Unmarshal(b, &pins) != nil {
		return ""
	}
	if p, ok := pins[source]; ok && p.Version == version {
		return p.SHA256
	}
	return ""
}

// config merges --config with --config-env values.
func (r *recorder) config() (string, error) {
	cfg := map[string]any{}
	if err := json.Unmarshal([]byte(r.o.config), &cfg); err != nil {
		return "", fmt.Errorf("--config: %w", err)
	}
	for _, ce := range r.o.configEnv {
		attr, env, ok := strings.Cut(ce, "=")
		if !ok {
			return "", fmt.Errorf("--config-env %q: want ATTR=ENV", ce)
		}
		v := os.Getenv(env)
		if len(v) < 8 {
			return "", fmt.Errorf("--config-env %s: $%s is unset or too short to scrub safely", attr, env)
		}
		cfg[attr] = v
		r.secrets[env] = v
	}
	b, err := json.Marshal(cfg)
	return string(b), err
}

func (r *recorder) importAndRead(ctx context.Context, typeName, id string) error {
	dir := filepath.Join(r.dir, typeName, slug(id))
	r.scrubber.Reset()
	req := &bridgev1.ImportRequest{Provider: r.ref, TypeName: typeName, Id: id}
	res, err := r.svc.Import(ctx, connect.NewRequest(req))
	if err != nil {
		fmt.Fprintf(os.Stderr, "import %s %s: %v\n", typeName, id, r.scrubErr(err))
		return r.write(filepath.Join(dir, "import.json"), req, nil, err)
	}
	// Keep the provider's state for the Read call; write a scrubbed copy.
	scrubbed := proto.Clone(res.Msg).(*bridgev1.ImportResponse)
	for _, rs := range scrubbed.GetResources() {
		if rs.StateJson, err = r.scrubState(rs.GetTypeName(), rs.GetStateJson()); err != nil {
			return err
		}
	}
	if err := r.write(filepath.Join(dir, "import.json"), req, scrubbed, nil); err != nil {
		return err
	}
	for i, rs := range res.Msg.GetResources() {
		rreq := &bridgev1.ReadRequest{Provider: r.ref, TypeName: rs.GetTypeName(), StateJson: rs.GetStateJson(), Private: rs.GetPrivate()}
		rres, rerr := r.svc.Read(ctx, connect.NewRequest(rreq))
		recReq := proto.Clone(rreq).(*bridgev1.ReadRequest)
		recReq.StateJson = scrubbed.GetResources()[i].GetStateJson()
		name := "read.json"
		if i > 0 {
			name = fmt.Sprintf("read-%d.json", i)
		}
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "read %s %s: %v\n", rs.GetTypeName(), id, r.scrubErr(rerr))
			if err := r.write(filepath.Join(dir, name), recReq, nil, rerr); err != nil {
				return err
			}
			continue
		}
		out := proto.Clone(rres.Msg).(*bridgev1.ReadResponse)
		if out.GetResource() != nil {
			if out.Resource.StateJson, err = r.scrubState(out.GetResource().GetTypeName(), out.GetResource().GetStateJson()); err != nil {
				return err
			}
		}
		if err := r.write(filepath.Join(dir, name), recReq, out, nil); err != nil {
			return err
		}
	}
	return nil
}

func (r *recorder) scrubState(typeName, stateJSON string) (string, error) {
	res, ok := r.schema.Resources[typeName]
	if !ok {
		return "", fmt.Errorf("schema has no resource type %s", typeName)
	}
	ty := res.Block.ImpliedType()
	v, err := values.FromJSON([]byte(stateJSON), ty)
	if err != nil {
		return "", fmt.Errorf("%s state: %w", typeName, err)
	}
	b, err := values.ToJSON(r.scrubber.State(typeName, res.Block, v), ty)
	return string(b), err
}

// scrubErr removes secrets from an error before it is printed.
func (r *recorder) scrubErr(err error) error {
	return errors.New(string(replaceSecrets([]byte(err.Error()), r.secrets)))
}

var marshal = protojson.MarshalOptions{}

func (r *recorder) write(path string, req, res proto.Message, callErr error) error {
	reqJSON, err := marshal.Marshal(req)
	if err != nil {
		return err
	}
	var resJSON []byte
	if callErr != nil {
		resJSON, err = errorJSON(callErr)
	} else {
		resJSON, err = marshal.Marshal(res)
	}
	if err != nil {
		return err
	}
	for _, msg := range []proto.Message{req, res} {
		if err := r.checkPrivate(msg); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	f := fixture{Provider: r.info, Request: reqJSON, Response: resJSON, Scrubbed: r.scrubber.Hits()}
	for env := range r.secrets {
		f.Scrubbed = append(f.Scrubbed, "$"+env)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	b = replaceSecrets(b, r.secrets)
	if err := r.assertClean(b); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := writeFile(path, append(b, '\n')); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// checkPrivate refuses opaque private bytes that contain a secret: they
// cannot be scrubbed without breaking the provider's round trip.
func (r *recorder) checkPrivate(msg proto.Message) error {
	var privs [][]byte
	switch m := msg.(type) {
	case *bridgev1.ImportResponse:
		for _, rs := range m.GetResources() {
			privs = append(privs, rs.GetPrivate())
		}
	case *bridgev1.ReadRequest:
		privs = append(privs, m.GetPrivate())
	case *bridgev1.ReadResponse:
		privs = append(privs, m.GetResource().GetPrivate())
	}
	for _, p := range privs {
		for env, secret := range r.secrets {
			if bytes.Contains(p, []byte(secret)) {
				return fmt.Errorf("private data contains $%s; refusing to record", env)
			}
		}
	}
	return nil
}

func (r *recorder) assertClean(b []byte) error {
	for env, secret := range r.secrets {
		for _, form := range secretForms(secret) {
			if bytes.Contains(b, []byte(form)) {
				return fmt.Errorf("$%s survived scrubbing; refusing to write", env)
			}
		}
	}
	return nil
}

// secretForms are the encodings in which a secret could appear in a fixture.
func secretForms(secret string) []string {
	escaped, _ := json.Marshal(secret)
	return []string{
		secret,
		string(escaped[1 : len(escaped)-1]),
		base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawURLEncoding.EncodeToString([]byte(secret)),
	}
}

func errorJSON(err error) ([]byte, error) {
	type diag = json.RawMessage
	out := struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Diagnostics []diag `json:"diagnostics"`
		} `json:"error"`
	}{}
	out.Error.Code = connect.CodeOf(err).String()
	out.Error.Message = err.Error()
	out.Error.Diagnostics = []diag{}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		out.Error.Message = cerr.Message()
		for _, d := range cerr.Details() {
			v, derr := d.Value()
			if derr != nil {
				continue
			}
			b, merr := marshal.Marshal(v)
			if merr != nil {
				return nil, merr
			}
			out.Error.Diagnostics = append(out.Error.Diagnostics, b)
		}
	}
	return json.Marshal(out)
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// slug turns an import ID into a directory name: "repo:main" -> "repo_main".
func slug(id string) string { return unsafe.ReplaceAllString(id, "_") }

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
