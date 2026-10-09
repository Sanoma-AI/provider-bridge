// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type testProvider struct{}

func newProvider() provider.Provider { return &testProvider{} }

type providerModel struct {
	APIKey types.String `tfsdk:"api_key"`
}

func (p *testProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "testprov"
	resp.Version = "0.0.0-test"
}

func (p *testProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Test provider for provider-bridge.",
		Attributes: map[string]schema.Attribute{
			"api_key": schema.StringAttribute{
				Description: "Any string; \"invalid\" makes Configure fail.",
				Optional:    true,
				Sensitive:   true,
			},
		},
	}
}

func (p *testProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.APIKey.ValueString() == "invalid" {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Invalid API key", "The API key was rejected.")
		return
	}
	resp.ResourceData = &client{apiKey: cfg.APIKey.ValueString()}
}

func (p *testProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{newItem}
}

func (p *testProvider) DataSources(context.Context) []func() datasource.DataSource { return nil }

type client struct{ apiKey string }
