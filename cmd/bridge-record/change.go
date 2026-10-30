// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	bridgev1 "github.com/Sanoma-AI/provider-bridge/gen/bridge/v1"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

// changeSpec is one object's --change steps: the first config creates it,
// each further one updates it.
type changeSpec struct {
	typeName, slug string
	configs        []string
}

// parseChanges groups --change TYPE=SLUG=CONFIG flags by TYPE=SLUG, in
// the order each first appears.
func parseChanges(flags []string) ([]*changeSpec, error) {
	var out []*changeSpec
	byKey := map[string]*changeSpec{}
	for _, f := range flags {
		typeName, rest, ok := strings.Cut(f, "=")
		name, config, ok2 := strings.Cut(rest, "=")
		var obj map[string]json.RawMessage
		if !ok || !ok2 || typeName == "" || name == "" || json.Unmarshal([]byte(config), &obj) != nil || obj == nil {
			return nil, fmt.Errorf("--change %q: want TYPE=SLUG=CONFIG with CONFIG a JSON object", f)
		}
		if slug(name) != name {
			return nil, fmt.Errorf("--change %q: SLUG may hold only letters, digits, '.', '_' and '-'", f)
		}
		key := typeName + "=" + name
		ch := byKey[key]
		if ch == nil {
			ch = &changeSpec{typeName: typeName, slug: name}
			byKey[key] = ch
			out = append(out, ch)
		}
		ch.configs = append(ch.configs, config)
	}
	return out, nil
}

// change records ch's steps, then deletes the object, if one exists, in a
// last step <slug>-delete: also after a failed step.
func (r *recorder) change(ctx context.Context, ch *changeSpec) (err error) {
	dir := filepath.Join(r.dir, ch.typeName)
	var cur *bridgev1.Resource
	defer func() {
		if cur != nil {
			_, _, derr := r.planApply(ctx, filepath.Join(dir, ch.slug+"-delete"), ch.typeName, cur, "")
			err = errors.Join(err, derr)
		}
	}()
	for i, config := range ch.configs {
		step := ch.slug
		if i > 0 {
			step = fmt.Sprintf("%s-update-%d", ch.slug, i)
		}
		var ok bool
		if cur, ok, err = r.planApply(ctx, filepath.Join(dir, step), ch.typeName, cur, config); err != nil || !ok {
			return err
		}
	}
	return nil
}

// planApply plans config (empty: delete) from prior and applies the plan,
// writing plan.json and apply.json into dir (and apply-delete.json first
// for a replace). It returns the object that exists afterwards, as far as
// the replies tell, and whether every call succeeded.
func (r *recorder) planApply(ctx context.Context, dir, typeName string, prior *bridgev1.Resource, config string) (*bridgev1.Resource, bool, error) {
	r.scrubber.Reset()
	preq := &bridgev1.PlanRequest{Provider: r.ref(), TypeName: typeName, ConfigJson: config}
	areq := &bridgev1.ApplyRequest{Provider: r.ref(), TypeName: typeName, ConfigJson: config}
	if prior != nil {
		version := prior.GetSchemaVersion()
		preq.PriorStateJson, preq.PriorPrivate, preq.PriorSchemaVersion = prior.GetStateJson(), prior.GetPrivate(), &version
		areq.PriorStateJson, areq.PriorPrivate, areq.PriorSchemaVersion = preq.PriorStateJson, preq.PriorPrivate, preq.PriorSchemaVersion
	}
	pres, err := r.svc.Plan(ctx, connect.NewRequest(preq))
	recReq, rerr := r.scrubPlanRequest(preq)
	if rerr != nil {
		return prior, false, rerr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan %s %s: %v\n", typeName, filepath.Base(dir), r.scrubErr(err))
		return prior, false, r.write(filepath.Join(dir, "plan.json"), recReq, nil, err)
	}
	recRes := proto.Clone(pres.Msg).(*bridgev1.PlanResponse)
	if recRes.PlannedStateJson, err = r.scrubState(typeName, recRes.GetPlannedStateJson()); err != nil {
		return prior, false, err
	}
	if recRes.Planned, err = r.scrubPlanned(typeName, recRes.GetPlanned()); err != nil {
		return prior, false, err
	}
	if err := r.write(filepath.Join(dir, "plan.json"), recReq, recRes, nil); err != nil {
		return prior, false, err
	}

	switch pres.Msg.GetAction() {
	case bridgev1.PlanResponse_ACTION_NONE:
		return prior, true, nil
	case bridgev1.PlanResponse_ACTION_REPLACE:
		del := &bridgev1.ApplyRequest{Provider: r.ref(), TypeName: typeName,
			PriorStateJson: areq.GetPriorStateJson(), PriorPrivate: areq.GetPriorPrivate(), PriorSchemaVersion: areq.PriorSchemaVersion, PlanToken: pres.Msg.GetPlanToken()}
		res, ok, err := r.apply(ctx, filepath.Join(dir, "apply-delete.json"), del)
		if err != nil || !ok {
			if res == nil {
				res = prior
			}
			return res, false, err
		}
		areq.PriorStateJson, areq.PriorPrivate, areq.PriorSchemaVersion = "", nil, nil
		prior = nil
	}
	areq.Planned, areq.PlannedPrivate = pres.Msg.GetPlanned(), pres.Msg.GetPlannedPrivate()
	areq.PlanToken = pres.Msg.GetPlanToken()
	res, ok, err := r.apply(ctx, filepath.Join(dir, "apply.json"), areq)
	if res == nil && !ok {
		// Failed without returning state: the object is as before.
		res = prior
	}
	return res, ok, err
}

// apply calls Apply and writes its reply to path. It returns the new state
// and whether the call succeeded without errors; the new state also when
// recording fails, so what was created is still deleted.
func (r *recorder) apply(ctx context.Context, path string, req *bridgev1.ApplyRequest) (*bridgev1.Resource, bool, error) {
	res, err := r.svc.Apply(ctx, connect.NewRequest(req))
	var live *bridgev1.Resource
	if err == nil {
		live = res.Msg.GetResource()
	}
	recReq := proto.Clone(req).(*bridgev1.ApplyRequest)
	var serr error
	if recReq.PriorStateJson, serr = r.scrubState(req.GetTypeName(), req.GetPriorStateJson()); serr != nil {
		return live, false, serr
	}
	if recReq.ConfigJson, serr = r.scrubState(req.GetTypeName(), req.GetConfigJson()); serr != nil {
		return live, false, serr
	}
	if recReq.Planned, serr = r.scrubPlanned(req.GetTypeName(), req.GetPlanned()); serr != nil {
		return live, false, serr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "apply %s: %v\n", path, r.scrubErr(err))
		return nil, false, r.write(path, recReq, nil, err)
	}
	recRes := proto.Clone(res.Msg).(*bridgev1.ApplyResponse)
	if recRes.Resource != nil {
		if recRes.Resource.StateJson, err = r.scrubState(req.GetTypeName(), recRes.GetResource().GetStateJson()); err != nil {
			return live, false, err
		}
	}
	for _, d := range res.Msg.GetErrors() {
		fmt.Fprintf(os.Stderr, "apply %s: %s: %s\n", path, d.GetAttributePath(), r.scrubErr(errors.New(d.GetSummary()+": "+d.GetDetail())))
	}
	return live, len(res.Msg.GetErrors()) == 0, r.write(path, recReq, recRes, nil)
}

func (r *recorder) scrubPlanRequest(req *bridgev1.PlanRequest) (*bridgev1.PlanRequest, error) {
	out := proto.Clone(req).(*bridgev1.PlanRequest)
	var err error
	if out.PriorStateJson, err = r.scrubState(req.GetTypeName(), req.GetPriorStateJson()); err != nil {
		return nil, err
	}
	out.ConfigJson, err = r.scrubState(req.GetTypeName(), req.GetConfigJson())
	return out, err
}

// scrubPlanned scrubs a msgpack plan, which may hold unknown values, and
// encodes it again; empty stays empty.
func (r *recorder) scrubPlanned(typeName string, mp []byte) ([]byte, error) {
	if len(mp) == 0 {
		return nil, nil
	}
	res, ok := r.schema.Resources[typeName]
	if !ok {
		return nil, fmt.Errorf("schema has no resource type %s", typeName)
	}
	ty := res.Block.ImpliedType()
	v, err := values.FromDynamic(mp, nil, ty)
	if err != nil {
		return nil, fmt.Errorf("%s plan: %w", typeName, err)
	}
	return values.ToMsgpack(r.scrubber.State(typeName, res.Block, v), ty)
}
