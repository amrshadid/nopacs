package deidjob

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// receptionFacts are what an instance's reception says that a rule's
// conditions read (engine.Reception), and which of them are known: the
// receiver from the reception's row; the calling AE title when its files
// came with exactly one; the called AE title from the receiver as it is
// configured now, a DIMSE receiver that still exists.
type receptionFacts struct {
	rec                                      engine.Reception
	receiverKnown, callingKnown, calledKnown bool
}

// receptionOf returns the facts of a reception, read once per unit. A
// reception the tenant no longer has knows nothing; an error is a database
// that does not answer.
func (j *judge) receptionOf(ctx context.Context, id string) (receptionFacts, error) {
	if f, ok := j.receptions[id]; ok || id == "" {
		return f, nil
	}
	var f receptionFacts
	r, _, err := j.p.Reception(ctx, id)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		j.receptions[id] = f
		return f, nil
	case err != nil:
		return f, err
	}
	f.rec.Receiver, f.receiverKnown = r.Receiver, r.Receiver != ""
	if len(r.CallingAETitles) == 1 && !r.CallingAETitlesTruncated {
		f.rec.CallingAETitle, f.callingKnown = r.CallingAETitles[0], true
	}
	if f.receiverKnown {
		rcv, err := j.p.Receiver(ctx, r.Receiver)
		switch {
		case err == nil && rcv.Type == tenant.ReceiverDIMSE:
			f.rec.CalledAETitle, f.calledKnown = rcv.DIMSE.AETitle, true
		case err != nil && fault.KindOf(err) != fault.NotFound:
			return f, err
		}
	}
	j.receptions[id] = f
	return f, nil
}

// conditionTags are the elements a condition's subject reads from the
// file (profile.proto, ConditionSubject); the others read the reception.
var conditionTags = map[v1.ConditionSubject]uint32{
	v1.ConditionSubject_CONDITION_SUBJECT_MODALITY:          0x00080060,
	v1.ConditionSubject_CONDITION_SUBJECT_SOP_CLASS_UID:     0x00080016,
	v1.ConditionSubject_CONDITION_SUBJECT_MANUFACTURER:      0x00080070,
	v1.ConditionSubject_CONDITION_SUBJECT_MODEL_NAME:        0x00081090,
	v1.ConditionSubject_CONDITION_SUBJECT_SOFTWARE_VERSIONS: 0x00181020,
}

// judgeable reports whether a condition's subject reads, from the archive,
// what it read from the file as received: a fact of the reception that is
// known, or an element of the file the version keeps (keptTop says, by
// tag, which top-level elements the archived file holds that the version
// keeps as they are). An element the version removed or changed no longer
// says what it said, so a condition on it cannot be judged.
func judgeable(subject v1.ConditionSubject, facts receptionFacts, keptTop map[uint32]bool) bool {
	switch subject {
	case v1.ConditionSubject_CONDITION_SUBJECT_RECEIVER:
		return facts.receiverKnown
	case v1.ConditionSubject_CONDITION_SUBJECT_CALLING_AE_TITLE:
		return facts.callingKnown
	case v1.ConditionSubject_CONDITION_SUBJECT_CALLED_AE_TITLE:
		return facts.calledKnown
	}
	t, ok := conditionTags[subject]
	return ok && keptTop[t]
}
