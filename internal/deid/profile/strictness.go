package profile

import (
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// Strictness is how much of an element's value an action lets through,
// ranked: a higher rank lets through less. Actions of one rank let
// through different things (a keyed hash and a shifted date, a constant
// and a dummy), so two of one rank are comparable only when identical.
type Strictness int

// The ranks, from the least strict.
const (
	// StrictKeep: keep, the value as it came.
	StrictKeep Strictness = iota + 1
	// StrictTransform: a value made from the original, which still says
	// something of it: hash, uid and lookup (a code the same for the same
	// value, so records stay linked), shift and generalize (a date or an
	// age made coarse), clean and redact (the text with what identifies
	// cut out, the rest kept).
	StrictTransform
	// StrictReplace: a value that says nothing of the original, but that
	// the element is there: replace, set and dummy.
	StrictReplace
	// StrictEmpty: the element stays with no value.
	StrictEmpty
	// StrictRemove: the element goes.
	StrictRemove
	// StrictQuarantine: the whole file is held.
	StrictQuarantine
)

// ActionStrictness returns the rank of an action that does one thing to
// every element, and false for one whose outcome depends on the element
// (remove_unless_required, whose outcome is its IOD's Type) or that this
// build does not know.
func ActionStrictness(a *v1.ProfileAction) (Strictness, bool) {
	switch a.GetKind().(type) {
	case *v1.ProfileAction_Keep:
		return StrictKeep, true
	case *v1.ProfileAction_Hash, *v1.ProfileAction_Uid, *v1.ProfileAction_Lookup,
		*v1.ProfileAction_Shift, *v1.ProfileAction_Generalize, *v1.ProfileAction_Clean, *v1.ProfileAction_Redact:
		return StrictTransform, true
	case *v1.ProfileAction_Replace, *v1.ProfileAction_Set, *v1.ProfileAction_Dummy:
		return StrictReplace, true
	case *v1.ProfileAction_Empty:
		return StrictEmpty, true
	case *v1.ProfileAction_Remove:
		return StrictRemove, true
	case *v1.ProfileAction_Quarantine:
		return StrictQuarantine, true
	}
	return 0, false
}

// Order is how an action compares with another for strictness.
type Order int

// The orders.
const (
	// OrderIncomparable: neither is known to be at least as strict as the
	// other (two actions of one rank that differ, or an action this build
	// does not know). A layer refuses it: what cannot be decided is not
	// let through.
	OrderIncomparable Order = iota
	// OrderLooser: the first lets through more than the second.
	OrderLooser
	// OrderSame: the two are the same action.
	OrderSame
	// OrderStricter: the first lets through less than the second.
	OrderStricter
)

// String names the order.
func (o Order) String() string {
	switch o {
	case OrderLooser:
		return "looser"
	case OrderSame:
		return "same"
	case OrderStricter:
		return "stricter"
	}
	return "incomparable"
}

// CompareActions compares upper with lower: whether upper, put in place of
// lower, lets through less (OrderStricter), the same (OrderSame), more
// (OrderLooser), or what cannot be decided (OrderIncomparable).
//
// remove_unless_required has an outcome per Type of the element in its IOD
// (Type 3 removed, Type 2 emptied, Type 1 its fallback: empty, dummy, or
// uid), so it is compared outcome by outcome, the same element having the
// same Type under both: upper is at least as strict when, for each Type,
// its outcome is. So remove_unless_required with the fallback empty is
// stricter than empty and looser than remove, and comparable with both.
// An outcome looser for any Type makes the whole looser, whatever the
// others, since it lets more through for those elements: empty over
// remove_unless_required with the fallback dummy is looser (it keeps a
// Type 3 element, emptied), and so is the other way (a dummy where empty
// writes nothing).
func CompareActions(upper, lower *v1.ProfileAction) Order {
	if proto.Equal(upper, lower) {
		return OrderSame
	}
	uo, uok := outcomes(upper)
	lo, lok := outcomes(lower)
	if !uok || !lok {
		return OrderIncomparable
	}
	stricter, incomparable := false, false
	for i := range uo {
		switch compareOutcome(uo[i], lo[i]) {
		case OrderLooser:
			return OrderLooser
		case OrderIncomparable:
			incomparable = true
		case OrderStricter:
			stricter = true
		}
	}
	switch {
	case incomparable:
		return OrderIncomparable
	case stricter:
		return OrderStricter
	}
	// Every outcome the same, the actions written apart: the same action.
	return OrderSame
}

// AtLeastAsStrict reports whether upper may stand in place of lower in a
// layer above it: the same action or a stricter one.
func AtLeastAsStrict(upper, lower *v1.ProfileAction) bool {
	o := CompareActions(upper, lower)
	return o == OrderSame || o == OrderStricter
}

// outcomes returns what an action does to an element of Type 3, Type 2
// and Type 1 in its IOD, each an action of one rank; false for an action
// this build does not know.
func outcomes(a *v1.ProfileAction) ([3]*v1.ProfileAction, bool) {
	rur := a.GetRemoveUnlessRequired()
	if rur == nil {
		if _, ok := ActionStrictness(a); !ok {
			return [3]*v1.ProfileAction{}, false
		}
		return [3]*v1.ProfileAction{a, a, a}, true
	}
	var fallback *v1.ProfileAction
	switch rur.GetFallback() {
	case v1.RequiredFallback_REQUIRED_FALLBACK_EMPTY:
		fallback = &v1.ProfileAction{Kind: &v1.ProfileAction_Empty{Empty: &v1.EmptyAction{}}}
	case v1.RequiredFallback_REQUIRED_FALLBACK_DUMMY:
		fallback = &v1.ProfileAction{Kind: &v1.ProfileAction_Dummy{Dummy: &v1.DummyAction{}}}
	case v1.RequiredFallback_REQUIRED_FALLBACK_UID:
		fallback = &v1.ProfileAction{Kind: &v1.ProfileAction_Uid{Uid: &v1.UidAction{}}}
	default:
		return [3]*v1.ProfileAction{}, false
	}
	empty := &v1.ProfileAction{Kind: &v1.ProfileAction_Empty{Empty: &v1.EmptyAction{}}}
	return [3]*v1.ProfileAction{removeAction, empty, fallback}, true
}

// compareOutcome compares two actions of one rank each.
func compareOutcome(upper, lower *v1.ProfileAction) Order {
	u, _ := ActionStrictness(upper)
	l, _ := ActionStrictness(lower)
	switch {
	case u > l:
		return OrderStricter
	case u < l:
		return OrderLooser
	case proto.Equal(upper, lower):
		return OrderSame
	}
	return OrderIncomparable
}
