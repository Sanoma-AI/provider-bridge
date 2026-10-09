// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"math/big"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// item is testprov_item. Its shape covers what the bridge must carry: a
// computed id, a required string, a map, a single nested attribute with a
// sensitive child and a big number, a list nested attribute, and a list
// nested block.
//
// Read derives the whole state from id: "gone" reads as deleted, "fail"
// returns an error diagnostic on name, anything else returns fixed values.
type item struct{ client *client }

func newItem() resource.Resource { return &item{} }

var (
	settingsType = map[string]attr.Type{"secret": types.StringType, "level": types.NumberType}
	ruleType     = map[string]attr.Type{"name": types.StringType, "port": types.Int64Type}
	endpointType = map[string]attr.Type{"url": types.StringType}
)

type itemModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Tags     types.Map    `tfsdk:"tags"`
	Settings types.Object `tfsdk:"settings"`
	Rules    types.List   `tfsdk:"rules"`
	Endpoint types.List   `tfsdk:"endpoint"`
}

func (r *item) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_item"
}

func (r *item) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A fake item whose state is derived from its id.",
		Attributes: map[string]schema.Attribute{
			"id":   schema.StringAttribute{Computed: true, Description: "Identifier."},
			"name": schema.StringAttribute{Required: true, Description: "Name."},
			"tags": schema.MapAttribute{Optional: true, ElementType: types.StringType},
			"settings": schema.SingleNestedAttribute{
				Optional: true,
				Attributes: map[string]schema.Attribute{
					"secret": schema.StringAttribute{Optional: true, Sensitive: true},
					"level":  schema.NumberAttribute{Optional: true},
				},
			},
			"rules": schema.ListNestedAttribute{
				Optional: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{Required: true},
					"port": schema.Int64Attribute{Optional: true},
				}},
			},
		},
		Blocks: map[string]schema.Block{
			"endpoint": schema.ListNestedBlock{
				NestedObject: schema.NestedBlockObject{Attributes: map[string]schema.Attribute{
					"url": schema.StringAttribute{Required: true},
				}},
			},
		},
	}
}

func (r *item) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if c, ok := req.ProviderData.(*client); ok {
		r.client = c
	}
}

func (r *item) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *item) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if r.client == nil {
		resp.Diagnostics.AddError("Provider not configured", "Configure must run before Read.")
		return
	}
	var state itemModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	switch id {
	case "gone":
		resp.State.RemoveResource(ctx)
		return
	case "fail":
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Read failed", "The item could not be read.")
		return
	}
	level, _ := new(big.Float).SetPrec(512).SetString("12345678901234567890.5")
	state = itemModel{
		ID:   types.StringValue(id),
		Name: types.StringValue("item-" + id),
		Tags: types.MapValueMust(types.StringType, map[string]attr.Value{"id": types.StringValue(id)}),
		Settings: types.ObjectValueMust(settingsType, map[string]attr.Value{
			"secret": types.StringValue("s3cret-" + id),
			"level":  types.NumberValue(level),
		}),
		Rules: types.ListValueMust(types.ObjectType{AttrTypes: ruleType}, []attr.Value{
			types.ObjectValueMust(ruleType, map[string]attr.Value{"name": types.StringValue("https"), "port": types.Int64Value(443)}),
		}),
		Endpoint: types.ListValueMust(types.ObjectType{AttrTypes: endpointType}, []attr.Value{
			types.ObjectValueMust(endpointType, map[string]attr.Value{"url": types.StringValue("https://example.invalid/" + id)}),
		}),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *item) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan itemModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	plan.ID = plan.Name
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *item) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan itemModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *item) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {}
