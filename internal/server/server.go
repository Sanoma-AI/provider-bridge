// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

// Package server exposes the provider Manager as the Connect service
// sanoma.providerbridge.v1.BridgeService over HTTP/1.1 and unencrypted
// HTTP/2 on a unix socket.
//
// Error diagnostics become a Connect error (FAILED_PRECONDITION,
// INVALID_ARGUMENT, UNAVAILABLE, NOT_FOUND) with one Diagnostic detail per
// diagnostic; warnings travel in the response.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	bridgev1 "github.com/Sanoma-AI/provider-bridge/gen/bridge/v1"
	"github.com/Sanoma-AI/provider-bridge/gen/bridge/v1/providerbridgev1connect"
	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/tfproto"
)

// Service implements BridgeService on a Manager.
type Service struct {
	m *provider.Manager
}

var _ providerbridgev1connect.BridgeServiceHandler = (*Service)(nil)

// NewService returns the service implementation, for in-process callers
// such as bridge-record.
func NewService(m *provider.Manager) *Service { return &Service{m: m} }

// NewHandler returns the HTTP handler serving BridgeService.
func NewHandler(m *provider.Manager, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	path, h := providerbridgev1connect.NewBridgeServiceHandler(NewService(m),
		connect.WithInterceptors(logInterceptor(log)))
	mux.Handle(path, h)
	return mux
}

func ref(p *bridgev1.ProviderRef) provider.Ref {
	return provider.Ref{Source: p.GetSource(), Version: p.GetVersion(), SHA256: p.GetSha256()}
}

// GetSchema implements BridgeService.
func (s *Service) GetSchema(ctx context.Context, req *connect.Request[bridgev1.GetSchemaRequest]) (*connect.Response[bridgev1.GetSchemaResponse], error) {
	res, err := s.m.Schema(ctx, ref(req.Msg.GetProvider()))
	if err != nil {
		return nil, toConnect(ctx, err)
	}
	return connect.NewResponse(&bridgev1.GetSchemaResponse{
		SchemaJson: res.JSON,
		Protocol:   int32(res.Schema.Protocol),
		Sha256:     res.SHA256,
	}), nil
}

// Configure implements BridgeService.
func (s *Service) Configure(ctx context.Context, req *connect.Request[bridgev1.ConfigureRequest]) (*connect.Response[bridgev1.ConfigureResponse], error) {
	warnings, sha, err := s.m.Configure(ctx, ref(req.Msg.GetProvider()), req.Msg.GetConfigJson())
	if err != nil {
		return nil, toConnect(ctx, err)
	}
	return connect.NewResponse(&bridgev1.ConfigureResponse{Warnings: diagnostics(warnings), Sha256: sha}), nil
}

// Import implements BridgeService.
func (s *Service) Import(ctx context.Context, req *connect.Request[bridgev1.ImportRequest]) (*connect.Response[bridgev1.ImportResponse], error) {
	res, warnings, err := s.m.Import(ctx, ref(req.Msg.GetProvider()), req.Msg.GetTypeName(), req.Msg.GetId())
	if err != nil {
		return nil, toConnect(ctx, err)
	}
	out := &bridgev1.ImportResponse{Warnings: diagnostics(warnings)}
	for _, r := range res {
		out.Resources = append(out.Resources, resource(r))
	}
	return connect.NewResponse(out), nil
}

// Read implements BridgeService.
func (s *Service) Read(ctx context.Context, req *connect.Request[bridgev1.ReadRequest]) (*connect.Response[bridgev1.ReadResponse], error) {
	res, gone, warnings, err := s.m.Read(ctx, ref(req.Msg.GetProvider()), req.Msg.GetTypeName(), []byte(req.Msg.GetStateJson()), req.Msg.GetPrivate(), req.Msg.SchemaVersion)
	if err != nil {
		return nil, toConnect(ctx, err)
	}
	out := &bridgev1.ReadResponse{Gone: gone, Warnings: diagnostics(warnings)}
	if !gone {
		out.Resource = resource(res)
	}
	return connect.NewResponse(out), nil
}

// Close implements BridgeService.
func (s *Service) Close(_ context.Context, req *connect.Request[bridgev1.CloseRequest]) (*connect.Response[bridgev1.CloseResponse], error) {
	s.m.Close(ref(req.Msg.GetProvider()))
	return connect.NewResponse(&bridgev1.CloseResponse{}), nil
}

func resource(r provider.Resource) *bridgev1.Resource {
	return &bridgev1.Resource{TypeName: r.TypeName, StateJson: string(r.State), Private: r.Private, SchemaVersion: r.SchemaVersion}
}

func diagnostics(ds tfproto.Diagnostics) []*bridgev1.Diagnostic {
	out := make([]*bridgev1.Diagnostic, 0, len(ds))
	for _, d := range ds {
		sev := bridgev1.Diagnostic_SEVERITY_ERROR
		if d.Severity == tfproto.SeverityWarning {
			sev = bridgev1.Diagnostic_SEVERITY_WARNING
		}
		out = append(out, &bridgev1.Diagnostic{Severity: sev, Summary: d.Summary, Detail: d.Detail, AttributePath: d.Attribute})
	}
	return out
}

var codes = map[provider.Code]connect.Code{
	provider.CodeInternal:           connect.CodeInternal,
	provider.CodeInvalidArgument:    connect.CodeInvalidArgument,
	provider.CodeFailedPrecondition: connect.CodeFailedPrecondition,
	provider.CodeUnavailable:        connect.CodeUnavailable,
	provider.CodeNotFound:           connect.CodeNotFound,
}

// toConnect maps a Manager error onto a Connect error with diagnostics.
func toConnect(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return connect.NewError(connect.CodeOf(ctxErr), err)
	}
	var perr *provider.Error
	if !errors.As(err, &perr) {
		return connect.NewError(connect.CodeInternal, err)
	}
	cerr := connect.NewError(codes[perr.Code], errors.New(perr.Error()))
	for _, d := range diagnostics(perr.Diagnostics) {
		if detail, derr := connect.NewErrorDetail(d); derr == nil {
			cerr.AddDetail(detail)
		}
	}
	return cerr
}

func logInterceptor(log *slog.Logger) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			res, err := next(ctx, req)
			attrs := []any{"procedure", req.Spec().Procedure, "duration_ms", time.Since(start).Milliseconds()}
			if err != nil {
				log.Warn("rpc failed", append(attrs, "code", connect.CodeOf(err).String(), "error", err.Error())...)
			} else {
				log.Info("rpc", attrs...)
			}
			return res, err
		}
	}
}
