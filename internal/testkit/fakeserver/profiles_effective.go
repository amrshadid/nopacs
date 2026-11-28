package fakeserver

import (
	"strconv"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
)

// effectiveOf answers DescribeProfile for a receiver's effective profile with
// the real language: the receiver's profile stacked over the tenant's
// deid.base-profile, when the tenant has one, each selector with the layer
// that decides it. The caller holds s.mu.
func (s *Server) effectiveOf(tenant, receiver string) (*v1.DescribeProfileResponse, error) {
	r, ok := s.arch.receivers[key(tenant, receiver)]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no receiver with this name")
	}
	ref := r.GetProfile()
	if ref == nil {
		return nil, fault.New(fault.Precondition, "effective_receiver: the receiver archives what it receives as received; it has no profile")
	}
	layerOf := func(kind profile.LayerKind, name string, version int32) (profile.Layer, error) {
		v, err := s.version(tenant, name, version)
		if err != nil {
			return profile.Layer{}, err
		}
		e, _ := proto.Clone(v.GetExpanded()).(*v1.ExpandedProfile)
		e.PixelRules = nil
		return profile.Layer{Kind: kind, Name: name + "@" + strconv.Itoa(int(v.GetVersion())), Expanded: e}, nil
	}
	upper, err := layerOf(profile.LayerReceiver, ref.GetName(), ref.GetVersion())
	if err != nil {
		return nil, err
	}
	layers := []profile.Layer{upper}
	if v, ok := s.settings[tenant][settings.DeidBaseProfile]; ok && v != "" {
		name, version, perr := settings.ParseBaseProfile(v)
		if perr != nil {
			return nil, fault.New(fault.Internal, "the fake's base profile is not name@version")
		}
		base, err := layerOf(profile.LayerTenant, name, version)
		if err != nil {
			return nil, err
		}
		layers = []profile.Layer{base, upper}
	}
	eff, problems := profile.Stack(layers...)
	if len(problems) > 0 {
		return nil, problemsRefusal(problems)
	}
	out := &v1.DescribeProfileResponse{Version: &v1.ProfileVersion{Name: ref.GetName(), Version: ref.GetVersion()}}
	for _, e := range eff.Describe() {
		out.Effective = append(out.Effective, &v1.EffectiveElement{
			What: e.What, Class: e.Class, Action: e.Action, Conditional: e.Conditional, Layer: e.Layer.String(), LayerProfile: e.LayerName,
			Rule: int32(e.Rule), RuleName: e.RuleName, Origin: e.Origin, //nolint:gosec // G115: a position within 10,000 rules.
		})
	}
	return out, nil
}
