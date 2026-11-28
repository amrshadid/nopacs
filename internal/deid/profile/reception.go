package profile

import (
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// Reception returns the profile a receiver runs when its tenant has a base:
// the receiver's profile stacked over the base (Stack), so an element the
// base decides and the receiver's profile does not name is decided by the
// base, and one both name by the stricter of the two layers' decisions, each
// made as the layer would alone (Program.Decide). Pixel rules are not layers
// (a layer above the first may have none): the result holds the base's, then
// the receiver's, and an image is decided by the base's rules when one
// matches it and by the receiver's only otherwise (Program.Pixels), so a
// receiver's rule never replaces what the base does to an image. Problems are those of Stack, for a receiver's profile that
// loosens or cannot be compared with the base; the result is then nil.
// Neither argument is changed.
func Reception(baseName string, base *v1.ExpandedProfile, receiverName string, receiver *v1.ExpandedProfile) (*v1.ExpandedProfile, Problems) {
	layer := func(kind LayerKind, name string, e *v1.ExpandedProfile) Layer {
		c, _ := proto.Clone(e).(*v1.ExpandedProfile)
		c.PixelRules = nil
		return Layer{Kind: kind, Name: name, Expanded: c}
	}
	eff, problems := Stack(layer(LayerTenant, baseName, base), layer(LayerReceiver, receiverName, receiver))
	if len(problems) > 0 {
		return nil, problems
	}
	out := eff.Expanded()
	for k, e := range []*v1.ExpandedProfile{base, receiver} {
		for _, r := range e.GetPixelRules() {
			c, _ := proto.Clone(r).(*v1.ExpandedPixelRule)
			out.PixelRules = append(out.PixelRules, c)
		}
		out.Layers[k].PixelRules = int32(len(e.GetPixelRules())) //nolint:gosec // G115: bounded by the profile's own limit.
	}
	return out, nil
}
