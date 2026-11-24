package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
)

// DiffProfileVersions answers what changes between two stored versions:
// bases, defaults, rules one by one and the effect on the expanded form
// (profile.Diff).
func (s profileService) DiffProfileVersions(ctx context.Context, req *v1.DiffProfileVersionsRequest) (*v1.DiffProfileVersionsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ProfileServiceDiffProfileVersionsProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inProfiles(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := profileName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	toName := req.GetToName()
	if toName == "" {
		toName = req.GetName()
	} else if err := profileName("to_name", toName); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetFromVersion() < 0 {
		return nil, c.failIn(d, st, invalid("from_version", "is 0 (the latest) or more"))
	}
	if req.GetToVersion() < 0 {
		return nil, c.failIn(d, st, invalid("to_version", "is 0 (the latest) or more"))
	}
	read := func(name string, version int32) (*v1.ProfileVersion, *v1.ExpandedProfile, error) {
		v, err := readVersion(ctx, p, name, version)
		if err != nil {
			return nil, nil, err
		}
		loaded, err := profile.Load(v.Name, v.Version, v.Source, v.Expanded, v.Checksum)
		if err != nil {
			return nil, nil, fault.Wrap(fault.Internal, err, "api: a stored profile version does not load")
		}
		v.Source, v.Expanded = nil, nil
		m, err := versionMessage(v, false)
		return m, loaded.Expanded(), err
	}
	fromMsg, from, err := read(req.GetName(), req.GetFromVersion())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	toMsg, to, err := read(toName, req.GetToVersion())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out := profile.Diff(from, to)
	out.From, out.To = fromMsg, toMsg
	c.done(d, st)
	return out, nil
}
