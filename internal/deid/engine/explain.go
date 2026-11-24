package engine

import (
	"context"

	"github.com/amrshadid/go-dicom/dataset"

	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// The reasons a rule whose selector matches an element does not decide it.
const (
	// WhyConditions: its conditions do not hold for the file.
	WhyConditions = "conditions"
	// WhyLessSpecific: a rule of a more specific selector decides.
	WhyLessSpecific = "less_specific"
	// WhyEarlier: a later rule of an equally specific selector decides.
	WhyEarlier = "earlier"
	// WhyFloor: the floor overrides every rule for this element.
	WhyFloor = "floor"
)

// Explanation is why one element of a file is treated as it is: its entry
// in the record, how the deciding rule selects it, and every other rule
// whose selector matches it with why that one does not decide. It holds no
// value of the file.
type Explanation struct {
	// Entry is the element's entry, as Apply records it.
	Entry Entry
	// SelectedBy is the deciding rule's tier (profile.Tier's word); empty
	// when no rule decides.
	SelectedBy string
	// Others are the other rules that match, by precedence.
	Others []Other
}

// Other is a rule whose selector matches an element and that does not
// decide it.
type Other struct {
	// Rule is its position in the expanded profile, from 0.
	Rule int
	// Name and Origin are the rule's.
	Name, Origin string
	// SelectedBy is its tier's word.
	SelectedBy string
	// WhyNot is one of the Why constants.
	WhyNot string
}

// Explain processes ds as Apply does, changing it in place, and answers
// for each element of the record why it is treated as it is. A file the
// profile cannot process returns Apply's *Failure, and ds as it was. The
// caller passes a data set it discards, since explaining applies nothing
// it keeps.
func (en *Engine) Explain(ctx context.Context, ds *dataset.Dataset, rec Reception) ([]Explanation, error) {
	if ds == nil {
		return nil, errNoDataset
	}
	// The facts conditions read are those of the file as received, read
	// before the engine changes it, as Apply reads them.
	f := readFacts(ds, rec)
	locs := map[int]profile.Location{}
	record, err := en.apply(ctx, ds, rec, Image{}, locs)
	if err != nil {
		return nil, err
	}
	pd, _ := en.dec.(*programDecider)
	out := make([]Explanation, len(record.Entries))
	for i, e := range record.Entries {
		out[i].Entry = e
		loc, asked := locs[i]
		if !asked || pd == nil {
			continue
		}
		matches := pd.prog.Matches(loc, f)
		var deciding profile.Tier
		for _, m := range matches {
			if m.Index == e.Rule {
				deciding = m.Tier
				out[i].SelectedBy = m.Tier.String()
			}
		}
		for _, m := range matches {
			if m.Index == e.Rule {
				continue
			}
			o := Other{Rule: m.Index, SelectedBy: m.Tier.String()}
			if er := pd.rules[m.Index]; er != nil {
				o.Name, o.Origin = er.GetRule().GetName(), er.GetOrigin()
			}
			switch {
			case !m.Holds:
				o.WhyNot = WhyConditions
			case e.Rule < 0:
				o.WhyNot = WhyFloor
			case m.Tier < deciding:
				o.WhyNot = WhyLessSpecific
			default:
				o.WhyNot = WhyEarlier
			}
			out[i].Others = append(out[i].Others, o)
		}
	}
	return out, nil
}
