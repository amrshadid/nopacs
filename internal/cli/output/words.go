package output

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The words below are how a person reads the API's enums, scopes and keys. The
// commands and the interactive program share them, so one value is never
// worded two ways. None of them cleans its result: a caller that draws text
// the server sent passes it through Clean.

// enumWord writes an enum value as a word for a person: its name without the
// prefix, in lowercase, so TENANT_STATE_READY is "ready".
func enumWord(name, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(name, prefix))
}

// Role is a role as a word; no role is "none".
func Role(r v1.Role) string {
	if r == v1.Role_ROLE_UNSPECIFIED {
		return "none"
	}
	return enumWord(r.String(), "ROLE_")
}

// Database is a database type as a word.
func Database(t v1.DatabaseType) string {
	if t == v1.DatabaseType_DATABASE_TYPE_UNSPECIFIED {
		return "unknown"
	}
	return enumWord(t.String(), "DATABASE_TYPE_")
}

// State is a tenant state as a word.
func State(s v1.TenantState) string {
	if s == v1.TenantState_TENANT_STATE_UNSPECIFIED {
		return "unknown"
	}
	return enumWord(s.String(), "TENANT_STATE_")
}

// Outcome is an audit outcome as a word.
func Outcome(o v1.AuditOutcome) string {
	if o == v1.AuditOutcome_AUDIT_OUTCOME_UNSPECIFIED {
		return "unknown"
	}
	return enumWord(o.String(), "AUDIT_OUTCOME_")
}

// AuditProblem is what an audit verification found wrong, as words with
// hyphens, so AUDIT_VERIFY_PROBLEM_KIND_CHAIN_BROKEN is "chain-broken".
func AuditProblem(k v1.AuditVerifyProblemKind) string {
	if k == v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_UNSPECIFIED {
		return "unknown"
	}
	return strings.ReplaceAll(enumWord(k.String(), "AUDIT_VERIFY_PROBLEM_KIND_"), "_", "-")
}

// Scope is a scope as words: "installation" or "tenant <name>".
func Scope(sc *v1.Scope) string {
	if t, ok := sc.GetKind().(*v1.Scope_Tenant); ok {
		return "tenant " + t.Tenant
	}
	if _, ok := sc.GetKind().(*v1.Scope_Installation); ok {
		return "installation"
	}
	return "unknown"
}

// SettingFrom says where a setting's value comes from, in words: "built-in
// default", "installation", or "tenant <name> (override)". A server that
// does not say (an older one) is read from the setting's scope.
func SettingFrom(st *v1.Setting) string {
	switch st.GetSource() {
	case v1.SettingSource_SETTING_SOURCE_BUILT_IN:
		return "built-in default"
	case v1.SettingSource_SETTING_SOURCE_INSTALLATION:
		return "installation"
	case v1.SettingSource_SETTING_SOURCE_TENANT:
		return Scope(st.GetScope()) + " (override)"
	}
	if st.GetScope().GetTenant() != "" {
		return Scope(st.GetScope()) + " (override)"
	}
	return "installation"
}

// KeyLabel is a key's id with its holder, for a sentence.
func KeyLabel(k *v1.Key) string {
	if k.GetHolder() == "" {
		return k.GetId()
	}
	return k.GetId() + " (" + k.GetHolder() + ")"
}

// KeyStatus says whether a key works at now: revoked, expired, or active. It
// is worked out from the times the server sent, which are all a client has;
// the server decides for itself on each request.
func KeyStatus(k *v1.Key, now time.Time) string {
	switch {
	case k.GetRevokeTime() != nil:
		return "revoked"
	case k.GetExpireTime() != nil && !k.GetExpireTime().AsTime().After(now):
		return "expired"
	}
	return "active"
}

// Method shortens a method's full name, "/nopacs.v1.TenantService/CreateTenant",
// to "TenantService/CreateTenant".
func Method(m string) string {
	return strings.TrimPrefix(m, "/nopacs.v1.")
}

// Who says who made a request: the key's id, the local operator's user, or
// nothing when the request carried neither.
func Who(e *v1.AuditEntry) string {
	switch {
	case e.GetKeyId() != "":
		return e.GetKeyId()
	case e.GetLocalUserId() != "":
		return "local:" + e.GetLocalUserId()
	}
	return "-"
}

// ParseSpan reads a length of time written as a number and a unit: s, m, h, d
// (24 hours) or w (7 days), such as 90d or 36h.
func ParseSpan(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("not a length of time")
	}
	unit := map[byte]time.Duration{
		's': time.Second, 'm': time.Minute, 'h': time.Hour,
		'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour,
	}[s[len(s)-1]]
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if unit == 0 || err != nil || n <= 0 || n > int64(time.Duration(1<<62)/unit) {
		return 0, fmt.Errorf("not a length of time")
	}
	return time.Duration(n) * unit, nil
}

// CheckKeyID refuses a value that is a whole key. A key id is public; a whole
// key is a secret, and one typed as an argument would be in the process list
// and in shell history, and one typed into a field would be drawn on the
// screen. The message never repeats the value.
func CheckKeyID(id string) error {
	if id == "" {
		return fault.New(fault.Invalid, "the key id is empty; \"nopacs key list\" shows the ids")
	}
	if strings.HasPrefix(id, "nopacs_") {
		return fault.New(fault.Invalid, "that is a whole key, which is a secret and is never an argument; give the key's id, which \"nopacs key list\" shows")
	}
	return nil
}

// StorageType is a storage's type as a word: "directory" or "s3", and
// "unknown" for a type this client was not built to know, which the contract
// says a client shows as such.
func StorageType(st *v1.Storage) string {
	switch st.GetType().(type) {
	case *v1.Storage_Directory:
		return "directory"
	case *v1.Storage_S3:
		return "s3"
	}
	return "unknown"
}

// StorageWhere says where a storage's files go, in words: a root and a path
// for a directory, an endpoint, a bucket and a prefix for S3. It holds no
// secret, since the server answers none.
func StorageWhere(st *v1.Storage) string {
	switch t := st.GetType().(type) {
	case *v1.Storage_Directory:
		return "root " + t.Directory.GetRoot() + ", path " + Dash(t.Directory.GetPath())
	case *v1.Storage_S3:
		return t.S3.GetEndpoint() + " bucket " + t.S3.GetBucket() + " prefix " + Dash(t.S3.GetPrefix())
	}
	return "-"
}

// StorageTestStep is a storage test step as a word.
func StorageTestStep(s v1.StorageTestStep) string {
	if s == v1.StorageTestStep_STORAGE_TEST_STEP_UNSPECIFIED {
		return "unknown"
	}
	return enumWord(s.String(), "STORAGE_TEST_STEP_")
}

// StorageTestOutcome is a storage test outcome as a word.
func StorageTestOutcome(o v1.StorageTestOutcome) string {
	if o == v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_UNSPECIFIED {
		return "unknown"
	}
	return enumWord(o.String(), "STORAGE_TEST_OUTCOME_")
}

// StorageFault is why a storage operation failed, as words with a space
// where the name has an underscore; "-" when there was no fault.
func StorageFault(f v1.StorageFault) string {
	if f == v1.StorageFault_STORAGE_FAULT_UNSPECIFIED {
		return "-"
	}
	return strings.ReplaceAll(enumWord(f.String(), "STORAGE_FAULT_"), "_", " ")
}

// ReceiverHealth says whether a receiver accepts data now: "up", "starting",
// or "down" with the reason and the server's detail. A client that finds a
// state or reason it does not know shows it as down for a reason it cannot
// name.
func ReceiverHealth(h *v1.ReceiverHealth) string {
	switch h.GetState() {
	case v1.ReceiverState_RECEIVER_STATE_UP:
		return "up"
	case v1.ReceiverState_RECEIVER_STATE_STARTING:
		return "starting"
	case v1.ReceiverState_RECEIVER_STATE_IDLE:
		// An import receiver holds no listener: it takes what its jobs
		// hand it.
		return "idle"
	case v1.ReceiverState_RECEIVER_STATE_DOWN:
		reason := "a reason this client cannot name"
		if _, known := v1.ReceiverDownReason_name[int32(h.GetDownReason())]; known && h.GetDownReason() != v1.ReceiverDownReason_RECEIVER_DOWN_REASON_UNSPECIFIED { // #nosec G115 -- an enum value
			reason = strings.ReplaceAll(enumWord(h.GetDownReason().String(), "RECEIVER_DOWN_REASON_"), "_", " ")
		}
		if h.GetDetail() != "" {
			reason += ": " + h.GetDetail()
		}
		return "down (" + reason + ")"
	}
	return "unknown"
}

// ReceptionStep is a reception's step as a word. A step this client does not
// know is a step in progress, as the contract says to read it.
func ReceptionStep(s v1.ReceptionStep) string {
	if s == v1.ReceptionStep_RECEPTION_STEP_UNSPECIFIED {
		return "unknown"
	}
	if _, ok := v1.ReceptionStep_name[int32(s)]; !ok { // #nosec G115 -- an enum value
		return "in progress"
	}
	return enumWord(s.String(), "RECEPTION_STEP_")
}

// QuarantineReason is why a file was quarantined, as words; a reason this
// client does not know is shown as one it cannot name.
func QuarantineReason(r v1.QuarantineReason) string {
	if r == v1.QuarantineReason_QUARANTINE_REASON_UNSPECIFIED {
		return "unknown"
	}
	if _, ok := v1.QuarantineReason_name[int32(r)]; !ok { // #nosec G115 -- an enum value
		return "a reason this client cannot name"
	}
	return strings.ReplaceAll(enumWord(r.String(), "QUARANTINE_REASON_"), "_", " ")
}

// QuarantineState is what has been decided about a quarantined file, as a
// word.
func QuarantineState(s v1.QuarantineState) string {
	if s == v1.QuarantineState_QUARANTINE_STATE_UNSPECIFIED {
		return "unknown"
	}
	return enumWord(s.String(), "QUARANTINE_STATE_")
}

// Bytes writes a count of bytes with its unit when a binary unit divides it
// whole (200GiB) and as plain bytes otherwise, the form the settings take.
func Bytes(n int64) string { return settings.FormatSize(n) }

// Human writes a size for a person: whole bytes below 1 KiB, and one
// decimal in the largest binary unit that keeps it at least 1 above.
func Human(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	v := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		v /= 1024
		if v < 1024 || u == "PiB" {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return ""
}
