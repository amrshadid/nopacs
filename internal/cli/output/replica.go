package output

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// These words are for the replicas, retention and holds of the archive. The
// commands and the interactive program share them, so a value is never worded
// two ways. None of them cleans text the server sent: a caller draws that
// through Clean.

// StorageTier is a storage's tier as a word. A tier this client does not know
// is shown as one that may not read at once, as the contract says to.
func StorageTier(t v1.StorageTier) string {
	switch t {
	case v1.StorageTier_STORAGE_TIER_UNSPECIFIED, v1.StorageTier_STORAGE_TIER_ONLINE:
		return "online"
	case v1.StorageTier_STORAGE_TIER_ARCHIVE:
		return "archive"
	}
	return "unknown (may not read at once)"
}

// RecallTier is how fast an archive-tier store is asked to bring an object
// back, as a word; the store's own default when it was not said.
func RecallTier(t v1.S3RecallTier) string {
	if t == v1.S3RecallTier_S3_RECALL_TIER_UNSPECIFIED {
		return "standard"
	}
	if _, ok := v1.S3RecallTier_name[int32(t)]; !ok { // #nosec G115 -- an enum value
		return "unknown"
	}
	return enumWord(t.String(), "S3_RECALL_TIER_")
}

// Encryption says whether a storage encrypts what it holds, never more
// strongly than the storage or its admin said: a directory's statement is
// "declared", not a fact the server checked. A value this client does not
// know is "unknown".
func Encryption(e v1.EncryptionAtRest, algorithm string) string {
	switch e {
	case v1.EncryptionAtRest_ENCRYPTION_AT_REST_NONE:
		return "no (the storage says it does not encrypt by default)"
	case v1.EncryptionAtRest_ENCRYPTION_AT_REST_ENCRYPTED:
		if algorithm != "" {
			return "yes, " + algorithm + " (the storage says so)"
		}
		return "yes (the storage says so)"
	case v1.EncryptionAtRest_ENCRYPTION_AT_REST_DECLARED:
		return "declared by the admin (not checked by the server)"
	case v1.EncryptionAtRest_ENCRYPTION_AT_REST_NOT_DECLARED:
		return "not declared"
	}
	return "unknown"
}

// Coverage words how far scrubs keep up with a storage: the share of its
// objects read whole within the scrub interval, with the overdue ones and the
// oldest unread; "-" when the server did not compute it.
func Coverage(c *v1.ScrubCoverage) string {
	if c == nil {
		return "-"
	}
	s := fmt.Sprintf("%.1f%% of %d objects read within the interval", c.GetShare()*100, c.GetObjects())
	if c.GetPresenceOnly() > 0 {
		s += fmt.Sprintf(", %d checked by presence only", c.GetPresenceOnly())
	}
	if c.GetOverdue() > 0 {
		s += fmt.Sprintf(", %d overdue, the oldest unread for %s", c.GetOverdue(), Span(c.GetOldestUnverified().AsDuration()))
	}
	return s
}

// Span writes a length of time in the largest of days, hours, minutes and
// seconds that keeps it at least one, such as "3d" or "90m".
func Span(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	case d >= time.Hour:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d >= time.Minute:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
}

// RetentionPeriod writes a retention period as years and days, "-" for none.
func RetentionPeriod(p *v1.RetentionPeriod) string {
	if p == nil || (p.GetYears() == 0 && p.GetDays() == 0) {
		return "-"
	}
	var parts []string
	if p.GetYears() > 0 {
		parts = append(parts, strconv.Itoa(int(p.GetYears()))+"y")
	}
	if p.GetDays() > 0 {
		parts = append(parts, strconv.Itoa(int(p.GetDays()))+"d")
	}
	return strings.Join(parts, " ")
}

// ParseRetentionPeriod reads a period a person types: whole years and days
// such as "7y", "30d" or "2y 90d" (spaces optional). It checks only the form;
// the bounds are the server's.
func ParseRetentionPeriod(s string) (*v1.RetentionPeriod, error) {
	p := &v1.RetentionPeriod{}
	rest := strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if rest == "" {
		return nil, fmt.Errorf("a period such as 7y, 90d or 2y90d")
	}
	seenY, seenD := false, false
	for rest != "" {
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 || i == len(rest) || i > 6 {
			return nil, fmt.Errorf("a period such as 7y, 90d or 2y90d")
		}
		n, err := strconv.ParseInt(rest[:i], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("a period such as 7y, 90d or 2y90d")
		}
		switch rest[i] {
		case 'y':
			if seenY {
				return nil, fmt.Errorf("years given twice")
			}
			seenY, p.Years = true, int32(n)
		case 'd':
			if seenD {
				return nil, fmt.Errorf("days given twice")
			}
			seenD, p.Days = true, int32(n)
		default:
			return nil, fmt.Errorf("a period such as 7y, 90d or 2y90d")
		}
		rest = rest[i+1:]
	}
	return p, nil
}

// RetentionAction is a rule's action as a word; an action this client does
// not know is "unknown".
func RetentionAction(a v1.RetentionAction) string {
	switch a {
	case v1.RetentionAction_RETENTION_ACTION_UNSPECIFIED, v1.RetentionAction_RETENTION_ACTION_REVIEW:
		return "review"
	case v1.RetentionAction_RETENTION_ACTION_DELETE:
		return "delete"
	}
	return "unknown"
}

// RetentionSelector says which sessions a rule matches, in words.
func RetentionSelector(s *v1.RetentionSelector) string {
	switch {
	case s.GetStorage() != "":
		return "storage " + s.GetStorage()
	case s.GetRoute() != "":
		return "receiver " + s.GetReceiver() + ", route " + s.GetRoute()
	case s.GetReceiver() != "":
		return "receiver " + s.GetReceiver()
	}
	return "every session"
}

// HoldState is a hold's state as a word.
func HoldState(s v1.HoldState) string {
	switch s {
	case v1.HoldState_HOLD_STATE_ACTIVE:
		return "active"
	case v1.HoldState_HOLD_STATE_RELEASED:
		return "released"
	}
	return "unknown"
}

// HoldTarget is what a hold covers, as a path of labels or "whole tenant".
func HoldTarget(t *v1.HoldTarget) string {
	if t.GetWholeTenant() {
		return "whole tenant"
	}
	parts := []string{t.GetSubjectLabel()}
	if t.GetSessionLabel() != "" {
		parts = append(parts, t.GetSessionLabel())
		if t.GetScanLabel() != "" {
			parts = append(parts, t.GetScanLabel())
		}
	}
	return strings.Join(parts, "/")
}

// ReplicationJobPairs lists what a replicate, repair or dispose job does as
// labeled values, and nothing for a job of another kind. Text the server sent
// (a storage name, a reason) is cleaned.
func ReplicationJobPairs(j *v1.Job) []Pair {
	switch k := j.GetKind().(type) {
	case *v1.Job_Replicate:
		r := k.Replicate
		how := "after an attach or a request"
		if r.GetScheduled() {
			how = "scheduled by the server"
		}
		return []Pair{
			{Label: "Copies to", Value: Clean(r.GetReplica())},
			{Label: "From primary", Value: Clean(r.GetPrimary())},
			{Label: "Origin", Value: how},
		}
	case *v1.Job_Repair:
		r := k.Repair
		what := strconv.Itoa(len(r.GetFindings())) + " findings"
		if r.GetAll() {
			what = "every finding that holds"
		}
		from := "any replica in step"
		if r.GetFrom() != "" {
			from = Clean(r.GetFrom())
		}
		how := "asked for by a person"
		if r.GetAutomatic() {
			how = "made by the server"
		}
		return []Pair{
			{Label: "Repairs", Value: Clean(r.GetStorage())},
			{Label: "Findings", Value: what},
			{Label: "Reads from", Value: from},
			{Label: "Origin", Value: how},
		}
	case *v1.Job_Dispose:
		d := k.Dispose
		how := "asked for by an admin"
		if d.GetScheduled() {
			how = "the daily pass"
		}
		return []Pair{
			{Label: "Disposes by rule", Value: Clean(d.GetRule())},
			{Label: "Sessions", Value: strconv.FormatInt(d.GetSessions(), 10)},
			{Label: "Disposal reason", Value: Clean(strings.TrimSpace(d.GetReason()))},
			{Label: "Origin", Value: how},
		}
	}
	return nil
}

// FindingKind is the kind of a scrub finding as a word, such as "damaged" or
// "manifest-damaged".
func FindingKind(k v1.ScrubFindingKind) string {
	if k == v1.ScrubFindingKind_SCRUB_FINDING_KIND_UNSPECIFIED {
		return "unknown"
	}
	return strings.ReplaceAll(enumWord(k.String(), "SCRUB_FINDING_KIND_"), "_", "-")
}

// PurgeClaim says what one storage did with a correction's purged objects,
// never more strongly than the storage said it.
func PurgeClaim(d *v1.StorageDisposition) string {
	switch {
	case d.GetObjectsHidden()+d.GetObjectsRetained()+d.GetObjectsUnknown() == 0:
		return strconv.FormatInt(d.GetObjectsRemoved(), 10) + " removed: the bytes are gone from this storage"
	case d.GetObjectsRetained() > 0:
		until := "-"
		if d.GetRetainUntil() != nil {
			until = d.GetRetainUntil().AsTime().UTC().Format(time.RFC3339)
		}
		return fmt.Sprintf("%d removed, %d retained by object lock until %s, %d hidden, %d unknown", d.GetObjectsRemoved(), d.GetObjectsRetained(),
			until, d.GetObjectsHidden(), d.GetObjectsUnknown())
	}
	return fmt.Sprintf("%d removed, %d hidden (older versions kept), %d unknown: the bytes may still be there", d.GetObjectsRemoved(),
		d.GetObjectsHidden(), d.GetObjectsUnknown())
}
