package api

import (
	"cmp"
	"slices"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The contract's values of what the store keeps of a correction, and back.
var (
	correctionKinds = map[record.Kind]v1.CorrectionKind{
		record.KindDelete:  v1.CorrectionKind_CORRECTION_KIND_DELETE,
		record.KindRelabel: v1.CorrectionKind_CORRECTION_KIND_RELABEL,
		record.KindReplace: v1.CorrectionKind_CORRECTION_KIND_REPLACE,
	}
	correctionStates = map[record.State]v1.CorrectionState{
		record.StatePrepared:  v1.CorrectionState_CORRECTION_STATE_PREPARED,
		record.StateCommitted: v1.CorrectionState_CORRECTION_STATE_COMMITTED,
		record.StatePurging:   v1.CorrectionState_CORRECTION_STATE_PURGING,
		record.StatePurged:    v1.CorrectionState_CORRECTION_STATE_PURGED,
		record.StateUndone:    v1.CorrectionState_CORRECTION_STATE_UNDONE,
		record.StateAbandoned: v1.CorrectionState_CORRECTION_STATE_ABANDONED,
	}
	heldKinds = map[string]v1.HeldValueKind{
		tenant.HeldSubject:  v1.HeldValueKind_HELD_VALUE_KIND_SUBJECT,
		tenant.HeldStudy:    v1.HeldValueKind_HELD_VALUE_KIND_STUDY,
		tenant.HeldSeries:   v1.HeldValueKind_HELD_VALUE_KIND_SERIES,
		tenant.HeldInstance: v1.HeldValueKind_HELD_VALUE_KIND_INSTANCE,
	}
	objectDispositions = map[blob.Disposition]v1.ObjectDisposition{
		blob.Removed:  v1.ObjectDisposition_OBJECT_DISPOSITION_REMOVED,
		blob.Hidden:   v1.ObjectDisposition_OBJECT_DISPOSITION_HIDDEN,
		blob.Retained: v1.ObjectDisposition_OBJECT_DISPOSITION_RETAINED,
		blob.Unknown:  v1.ObjectDisposition_OBJECT_DISPOSITION_UNKNOWN,
	}
	resolvedDispositions = map[string]v1.InstanceDisposition{
		tenant.ResolvedCurrent:    v1.InstanceDisposition_INSTANCE_DISPOSITION_CURRENT,
		tenant.ResolvedSuperseded: v1.InstanceDisposition_INSTANCE_DISPOSITION_SUPERSEDED,
		tenant.ResolvedDeleted:    v1.InstanceDisposition_INSTANCE_DISPOSITION_DELETED,
		tenant.ResolvedUnknown:    v1.InstanceDisposition_INSTANCE_DISPOSITION_UNKNOWN,
	}
	findingKinds = map[tenant.ScrubFindingKind]v1.ScrubFindingKind{
		tenant.FindingMissing:         v1.ScrubFindingKind_SCRUB_FINDING_KIND_MISSING,
		tenant.FindingOrphan:          v1.ScrubFindingKind_SCRUB_FINDING_KIND_ORPHAN,
		tenant.FindingDamaged:         v1.ScrubFindingKind_SCRUB_FINDING_KIND_DAMAGED,
		tenant.FindingDisagreeing:     v1.ScrubFindingKind_SCRUB_FINDING_KIND_DISAGREEING,
		tenant.FindingManifestDamaged: v1.ScrubFindingKind_SCRUB_FINDING_KIND_MANIFEST_DAMAGED,
	}
)

// mapKey returns the key of m whose value is v, and whether there is one.
func mapKey[K comparable, V comparable](m map[K]V, v V) (K, bool) {
	for k, x := range m {
		if x == v {
			return k, true
		}
	}
	var zero K
	return zero, false
}

// targetOf returns the target a request names, as the store takes it.
func targetOf(t *v1.CorrectionTarget) tenant.CorrectionTarget {
	return tenant.CorrectionTarget{Subject: t.GetSubjectLabel(), Session: t.GetSessionLabel(), Scan: t.GetScanLabel(), Instance: t.GetInstanceId()}
}

// targetMessage returns a target as the contract writes it.
func targetMessage(t tenant.CorrectionTarget) *v1.CorrectionTarget {
	return &v1.CorrectionTarget{SubjectLabel: t.Subject, SessionLabel: t.Session, ScanLabel: t.Scan, InstanceId: t.Instance}
}

// correctionCounts returns a correction's counts as the contract writes them.
func correctionCounts(c tenant.CorrectionCounts) *v1.CorrectionCounts {
	return &v1.CorrectionCounts{Subjects: c.Subjects, Sessions: c.Sessions, Scans: c.Scans, Instances: c.Instances, Bytes: c.Bytes,
		ObjectsRemoved: c.ObjectsRemoved, BytesRemoved: c.BytesRemoved, ObjectsCopied: c.ObjectsCopied,
		ObjectsSuperseded: c.ObjectsSuperseded, ObjectsKept: c.ObjectsKept, ObjectsOwned: c.ObjectsOwned}
}

// peersMessage returns the senders a correction's data was sent to.
func peersMessage(peers []tenant.PeerCopy) []*v1.PeerCopy {
	out := make([]*v1.PeerCopy, 0, len(peers))
	for _, p := range peers {
		out = append(out, &v1.PeerCopy{Sender: p.Sender, Instances: p.Instances, LastSendTime: timestamp(p.LastSend)})
	}
	return out
}

// holdsMessage returns the values a delete holds arrivals by, ordered by
// the contract's order of kinds, then value.
func holdsMessage(holds []tenant.HeldValue) []*v1.HeldValue {
	out := make([]*v1.HeldValue, 0, len(holds))
	for _, h := range holds {
		out = append(out, &v1.HeldValue{Kind: heldKinds[h.Kind], Value: h.Value})
	}
	slices.SortFunc(out, func(a, b *v1.HeldValue) int {
		return cmp.Or(cmp.Compare(a.GetKind(), b.GetKind()), cmp.Compare(a.GetValue(), b.GetValue()))
	})
	return out
}

// correctionMessage returns a correction as the contract writes it. Its
// holds are given only when withHolds, as for a read of one correction.
func correctionMessage(c tenant.Correction, withHolds bool) *v1.Correction {
	m := &v1.Correction{
		Id: c.ID, Kind: correctionKinds[c.Kind], State: correctionStates[c.State], Target: targetMessage(c.Target),
		ToLabel: c.ToLabel, QuarantinedFile: c.Quarantine, Reason: c.Reason, CreatedByKeyId: c.KeyID,
		CreateTime: timestamp(c.Created), EndTime: timestamp(c.Ended), Counts: correctionCounts(c.Counts),
		StateReason: c.StateReason, ArrivalsAllowed: c.ArrivalsAllowed, UndoReason: c.UndoReason,
		Unresolved: c.Unresolved, Origin: v1.CorrectionOrigin_CORRECTION_ORIGIN_PERSON, Swept: c.Swept,
	}
	// A purge time is set from the commit on, and said only while the
	// correction can still reach its purge, never for one unresolved.
	if c.State == record.StateCommitted || c.State == record.StatePurging || c.State == record.StatePurged {
		if !c.Unresolved {
			m.PurgeTime = timestamp(c.PurgeAfter)
		}
	}
	if c.State != record.StatePrepared && c.State != record.StateAbandoned {
		m.CommitTime = timestamp(c.CommitTime)
	}
	if c.Kind == record.KindReplace {
		m.OldSha256, m.NewSha256 = c.OldSHA256.String(), c.NewSHA256.String()
		m.OldRevision, m.NewRevision = int32(c.OldRevision), int32(c.NewRevision) //nolint:gosec // G115: a revision is at most layout.MaxRevision.
	}
	for _, d := range c.Dispositions {
		m.Dispositions = append(m.Dispositions, &v1.StorageDisposition{Storage: d.Storage, ObjectsRemoved: d.Removed, ObjectsHidden: d.Hidden,
			ObjectsRetained: d.Retained, ObjectsUnknown: d.Unknown, VersionsRemoved: d.VersionsRemoved, RetainUntil: timestamp(d.RetainUntil)})
	}
	if withHolds && c.Kind == record.KindDelete {
		m.Holds = holdsMessage(c.Holds)
	}
	return m
}

// previewMessage returns a delete's preview as the contract writes it.
func previewMessage(p correct.Preview) *v1.DeletePreview {
	m := &v1.DeletePreview{
		Counts: correctionCounts(p.Counts), Storages: p.Storages, SentTo: peersMessage(p.SentTo), Jobs: p.Jobs,
		JobsNotListed: p.JobsNotListed, Confirm: p.Confirm, PurgeTime: timestamp(p.PurgeAfter),
		Quarantined: p.Quarantined, Receptions: p.Receptions, HeldAfterPurge: p.HeldAfterPurge, TenantJobs: p.TenantJobs,
	}
	if m.Storages == nil {
		m.Storages = []string{}
	}
	for _, sp := range p.Purges {
		m.StoragePurges = append(m.StoragePurges, &v1.StoragePurge{Storage: sp.Storage, Expected: objectDispositions[sp.Expected],
			Why: clip(sp.Why, tenant.MaxStateReasonLength-3)})
	}
	return m
}

// findingMessage returns a scrub finding as the contract writes it.
func findingMessage(f tenant.ScrubFinding) *v1.ScrubFinding {
	m := &v1.ScrubFinding{
		Id: f.ID, Kind: findingKinds[f.Kind], Storage: f.Storage, Key: f.Key, SubjectLabel: f.Path.Subject,
		SessionLabel: f.Path.Session, ScanLabel: f.Path.Scan, InstanceId: f.SOPInstanceUID,
		ExpectedSize: f.ExpectedSize, FoundSize: f.FoundSize, Job: f.Job, FindTime: timestamp(f.Found),
		ClearTime: timestamp(f.Cleared), ClearJob: f.ClearJob, Revision: int32(f.Revision), //nolint:gosec // G115: a finding's revision is at most a million.
	}
	if !f.ExpectedSHA256.IsZero() {
		m.ExpectedSha256 = f.ExpectedSHA256.String()
	}
	if !f.FoundSHA256.IsZero() {
		m.FoundSha256 = f.FoundSHA256.String()
	}
	return m
}

// resolvedMessage returns what became of one pair, as the contract writes
// it.
func resolvedMessage(r tenant.ResolvedInstance) *v1.ResolvedInstance {
	m := &v1.ResolvedInstance{
		Member:      &v1.InstanceRef{InstanceId: r.Ref.UID, Revision: int32(r.Ref.Revision)}, //nolint:gosec // G115: checked at most layout.MaxRevision.
		Disposition: resolvedDispositions[r.Disposition],
	}
	switch r.Disposition {
	case tenant.ResolvedCurrent, tenant.ResolvedSuperseded:
		m.CurrentRevision = int32(r.CurrentRevision) //nolint:gosec // G115: a revision is at most layout.MaxRevision.
		if !r.SHA256.IsZero() {
			m.Sha256 = r.SHA256.String()
		}
		m.SubjectLabel, m.SessionLabel, m.ScanLabel = r.Path.Subject, r.Path.Session, r.Path.Scan
	}
	switch r.Disposition {
	case tenant.ResolvedSuperseded, tenant.ResolvedDeleted:
		m.Correction, m.BytesKept = r.Correction, r.BytesKept
	}
	return m
}

// countDetails adds what a delete covers to its audit details.
func countDetails(d map[string]string, c tenant.CorrectionCounts) {
	for k, v := range map[string]int64{"subjects": c.Subjects, "sessions": c.Sessions, "scans": c.Scans, "instances": c.Instances, "bytes": c.Bytes} {
		d[k] = strconv.FormatInt(v, 10)
	}
}
