package fault

// The reasons: the fixed words the API contract names for a case inside a
// kind (access.proto, "Where a kind above names a reason"). Each travels as
// the reason of a google.rpc.ErrorInfo detail, the gRPC richer error model's
// message for "which case of this code is it", and begins the error's
// message, "REASON: ...", so a person reading a log sees it without decoding
// anything. They are written here, once, because the packages that refuse
// with them sit below the API (the master's and the tenant's ports, the
// storage switch gate) and cannot import it; the API's codes package reads
// this table to give each its detail.
//
// A reason never names a kind on its own: an error of a kind with no case
// of its own carries the kind's general reason, which the codes package
// derives from the code (NOT_FOUND, UNAVAILABLE, ...), not one of these.
const (
	// ReasonTenantMoved is a change to a tenant an export handed to another
	// installation, which is read only here.
	ReasonTenantMoved = "TENANT_MOVED"
	// ReasonTenantFenced is a change to, or a read of the data of, a tenant
	// whose storages name another installation or a later generation.
	ReasonTenantFenced = "TENANT_FENCED"
	// ReasonHeld is a delete of data a hold covers.
	ReasonHeld = "HELD"
	// ReasonRetentionMinimum is a person's delete of data a retention rule
	// keeps at least until a time to come.
	ReasonRetentionMinimum = "RETENTION_MINIMUM"
	// ReasonRelabeled is a lookup by labels a committed relabel moved.
	ReasonRelabeled = "RELABELED"
	// ReasonTenantRestoring is a tenant being reconciled after a restore.
	ReasonTenantRestoring = "TENANT_RESTORING"
	// ReasonNotOnline is an object its storage holds in a tier it cannot
	// read at once, with no copy in step online elsewhere.
	ReasonNotOnline = "NOT_ONLINE"
	// ReasonStorageSwitching is a write to a storage being switched to its
	// replica, which waits for the swap.
	ReasonStorageSwitching = "STORAGE_SWITCHING"
	// ReasonCursorExpired is an event cursor before the oldest event kept.
	ReasonCursorExpired = "CURSOR_EXPIRED"
	// ReasonHistoryChanged is an event cursor of another history of the
	// tenant's feed, or past its newest event.
	ReasonHistoryChanged = "EVENT_HISTORY_CHANGED"
)

// Case is one entry of the table of reasons: the reason, the one kind it is
// a case of, and the metadata keys its google.rpc.ErrorInfo may carry.
type Case struct {
	// Reason is the fixed word.
	Reason string
	// Kind is the kind the reason is a case of; an error of another kind
	// never carries it.
	Kind Kind
	// Metadata are the keys of the facts the case gives, when it knows
	// them; empty for a case that gives none.
	Metadata []string
}

// cases is the table. It is the only place a reason is paired with its kind
// and its metadata keys.
var cases = [...]Case{
	{ReasonTenantMoved, Precondition, nil},
	{ReasonTenantFenced, Precondition, nil},
	{ReasonHeld, Precondition, []string{"hold"}},
	{ReasonRetentionMinimum, Precondition, []string{"keep_until"}},
	{ReasonRelabeled, NotFound, []string{"correction", "to_label"}},
	{ReasonTenantRestoring, Unavailable, nil},
	{ReasonNotOnline, Unavailable, []string{"recall_state"}},
	{ReasonStorageSwitching, Unavailable, nil},
	{ReasonCursorExpired, OutOfRange, nil},
	{ReasonHistoryChanged, OutOfRange, nil},
}

// Cases returns every reason of the table, in its order. The slice is a
// copy, so a caller cannot change the table.
func Cases() []Case {
	out := make([]Case, len(cases))
	for i, c := range cases {
		c.Metadata = append([]string(nil), c.Metadata...)
		out[i] = c
	}
	return out
}

// CaseOf returns the table's entry for reason, and false for a word the
// table does not hold.
func CaseOf(reason string) (Case, bool) {
	for _, c := range cases {
		if c.Reason == reason {
			c.Metadata = append([]string(nil), c.Metadata...)
			return c, true
		}
	}
	return Case{}, false
}
