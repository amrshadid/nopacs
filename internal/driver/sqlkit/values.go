package sqlkit

import (
	"encoding/json"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// JSONText writes v as JSON for a column that is stored and returned whole
// and never searched inside, so it is the same text on every database.
func JSONText(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "a value cannot be written as JSON")
	}
	return string(b), nil
}

// FromJSON reads a column JSONText wrote into v. A column that does not
// read is the server's fault: only JSONText writes one.
func FromJSON(s string, v any) error {
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fault.Wrap(fault.Internal, err, "a stored value cannot be read")
	}
	return nil
}

// ToSHA256 reads a checksum column; NULL or a value of another length is
// none (the zero checksum), which a column's own check never lets in.
func ToSHA256(b []byte) tenant.SHA256 {
	var s tenant.SHA256
	if len(b) == len(s) {
		copy(s[:], b)
	}
	return s
}

// NullText returns NULL for the empty string, for a column where no value
// is NULL rather than "" (a grant's tenant_id on the installation).
func NullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// EncodeDetails writes an audit entry's details as a JSON object. JSON is
// stored and returned whole and never searched inside, so it is the same in
// every SQL database, with no dialect's JSON functions to keep correct.
// encoding/json sorts the keys, so an entry's text does not depend on the
// map's order.
func EncodeDetails(d map[string]string) (string, error) {
	if len(d) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "the audit entry's details cannot be written")
	}
	return string(b), nil
}

// DecodeDetails reads what EncodeDetails wrote; no details is nil.
func DecodeDetails(s string) (map[string]string, error) {
	var d map[string]string
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "an audit entry's details cannot be read")
	}
	if len(d) == 0 {
		return nil, nil
	}
	return d, nil
}

// DistinctTypes returns types without repeats, in their first order: a type
// named twice in a read of events would be read twice.
func DistinctTypes(types []tenant.EventType) []tenant.EventType {
	var out []tenant.EventType
	for _, typ := range types {
		seen := false
		for _, o := range out {
			seen = seen || o == typ
		}
		if !seen {
			out = append(out, typ)
		}
	}
	return out
}

// ReleaseEntry returns a release's audit entry with the reception the file
// joined, when it joined one.
func ReleaseEntry(audit master.AuditEntry, joined string) (master.AuditEntry, error) {
	if joined == "" {
		return audit, nil
	}
	return AddDetails(audit, tenant.DetailReleaseReception, joined)
}

// AddDetails returns the audit entry with the details a store adds, checked
// with the rest of the entry: a caller's entry already at its bound of
// details is refused before anything is written, never cut.
func AddDetails(audit master.AuditEntry, pairs ...string) (master.AuditEntry, error) {
	a := audit.WithDetails(pairs...)
	return a, a.Validate()
}
