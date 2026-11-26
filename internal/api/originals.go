package api

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a caller reads may leave the site: an AI agent sends whatever a
// command prints to the model's vendor. So the values a preview or a trial
// holds (what the sender sent, before de-identification) are answered only
// when asked for (show_values, audited), and a masked key, the key such a
// service holds (access.Principal.Masked), is never answered a value that
// is not in its archived, de-identified form. A request that holds
// originals whatever it names says so in its requirement
// (access.Requirement.AnswersOriginals) and the check refuses it; what only
// the handler can tell holds originals goes through
// access.Decision.RefuseOriginals, here and in each handler.

// A masked key is also refused what decides where the archive's bytes go
// (access.Requirement.ChoosesDestination): a send, a replica, a storage, a
// sender, a receiver's forwarding. The service that holds such a key is
// outside the site; where data goes is a person's choice.

// receiverChangeNeed returns the requirement of adding the receiver rcv
// (paths nil) or of changing the fields paths name to rcv's values, on the
// tenant named name: admin, refused to a masked key when the change makes
// the receiver forward, by forward_to or by a route's senders. A change
// that empties both, or names neither, is not.
func receiverChangeNeed(name string, rcv *v1.Receiver, paths []string) access.Requirement {
	need := onTenantNamed(name, master.RoleAdmin)
	names := func(p string) bool { return paths == nil || slices.Contains(paths, p) }
	forwards := names("forward_to") && len(rcv.GetForwardTo()) > 0
	if names("routing") {
		for _, r := range rcv.GetRouting().GetRoutes() {
			if len(r.GetForwardTo().GetSenders()) > 0 {
				forwards = true
			}
		}
	}
	if forwards {
		need = need.ChoosesDestination()
	}
	return need
}

// detailShowValues is the detail of a request that asked for values a
// preview hides by default.
const detailShowValues = "show_values"

// showValuesDetails returns details with show_values recorded when the
// request asked for values; a copy, never details itself, and nil for nil
// details when it did not.
func showValuesDetails(show bool, details map[string]string) map[string]string {
	if !show {
		return maps.Clone(details)
	}
	out := maps.Clone(details)
	if out == nil {
		out = map[string]string{}
	}
	out[detailShowValues] = "true"
	return out
}

// refuseAsReceived returns the refusal of a masked key reading in, an
// instance the catalog holds as it was received (no profile version
// de-identified it), so its header and bytes are the sender's originals;
// nil for an instance archived de-identified, and for any other caller.
func refuseAsReceived(d access.Decision, in tenant.ScanInstance) error {
	if in.Profile.IsZero() {
		return d.RefuseOriginals()
	}
	return nil
}

// recordFor returns the quarantine record f as the contract writes it for
// the caller of d: the path an import read for a caller who may read paths
// (mayReadPaths), and, for a masked key, without what holds an original
// value: the UIDs as received and every detail that holds one.
func recordFor(d access.Decision, f tenant.QuarantinedFile) *v1.QuarantinedFile {
	m := fileMessage(f)
	if mayReadPaths(d) {
		m.ImportPath = f.ImportPath
	}
	if d.Masked() {
		maskRecord(m)
	}
	return m
}

// maskRecord removes from m what holds an original value: the Study,
// Series and SOP Instance UIDs as received, and each detail that holds one
// (a detail holds no value of the file but a UID as received,
// quarantine.proto). The SOP Class and Transfer Syntax UIDs name a kind of
// object and an encoding, never a person, and stay.
func maskRecord(m *v1.QuarantinedFile) {
	m.StudyInstanceUid, m.SeriesInstanceUid, m.SopInstanceUid = "", "", ""
	for k := range m.Details {
		if strings.HasSuffix(k, "_instance_uid") {
			delete(m.Details, k)
		}
	}
}

// headerWithoutValues returns a header in the DICOM JSON model (PS3.18
// F.2) with every attribute's value removed: each attribute keeps its "vr"
// and, for a sequence, its items, each stripped the same way; "Value",
// "InlineBinary" and "BulkDataURI" are dropped at every depth. The text is
// the server's own JSON, never a file's bytes: DICOM is read by the DICOM
// library before it. Members are written in the order of their tags, as
// the model orders them. A header that is not such JSON is an error, and
// the caller answers it as a header it cannot give.
func headerWithoutValues(text string) (string, error) {
	var ds map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &ds); err != nil {
		return "", err
	}
	b, err := json.Marshal(stripDataset(ds))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stripDataset is headerWithoutValues for one data set. A member that does
// not read as the model writes it is left out, never kept as it is.
func stripDataset(ds map[string]map[string]json.RawMessage) map[string]any {
	out := make(map[string]any, len(ds))
	for tag, attr := range ds {
		a := map[string]any{}
		var vr string
		if raw, ok := attr["vr"]; ok && json.Unmarshal(raw, &vr) == nil {
			a["vr"] = vr
		}
		if raw, ok := attr["Value"]; ok && vr == "SQ" {
			var items []map[string]map[string]json.RawMessage
			if json.Unmarshal(raw, &items) == nil {
				stripped := make([]any, len(items))
				for i, it := range items {
					stripped[i] = stripDataset(it)
				}
				a["Value"] = stripped
			}
		}
		out[tag] = a
	}
	return out
}
