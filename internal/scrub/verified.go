package scrub

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A storage's coverage says how much of what it holds a scrub has read
// whole lately: the share of its objects verified within scrub.interval,
// how long the object waiting longest has gone unverified, and how many
// objects are overdue. It is what tells a person that a scrub keeps up: at
// 64 MiB a second a scrub reads 300 TB in 51.7 days, so a storage whose
// interval is shorter than that never reaches full coverage, and its
// overdue count says by how much.
//
// It is computed from when each scan was last read whole on each storage,
// which a scan's unit records at its end on every copy it read
// (tenant.VerificationRecorder, which the tenant's database keeps), never
// from the job counts: a scrub of one storage, a scrub of the whole tenant
// and a request to scrub one instance all read objects, and only the time
// of the read says whether an object is covered.

// ScanVerified is tenant.ScanVerified, the record of one copy of a scan a
// unit read to its end.
type ScanVerified = tenant.ScanVerified

// VerificationRecorder is tenant.VerificationRecorder, which records when
// each scan was last read whole on each storage.
type VerificationRecorder = tenant.VerificationRecorder

// ScanVerification is tenant.ScanVerification, what is recorded of one
// scan on one storage, as Coverage reads it.
type ScanVerification = tenant.ScanVerification

// VerificationSource is tenant.VerificationSource, which answers what is
// recorded of the scans of a storage, a page at a time.
type VerificationSource = tenant.VerificationSource

// StorageCoverage is one storage's coverage.
type StorageCoverage struct {
	// Storage names it.
	Storage string
	// Objects counts the objects it holds; Verified those read whole
	// within the interval, and Share their part of Objects (1 when it holds
	// none).
	Objects, Verified int64
	Share             float64
	// PresenceOnly counts the objects checked within the interval by
	// presence only, not counted in Verified.
	PresenceOnly int64
	// OldestUnverified is how long the object waiting longest has gone
	// unread, from its last read or, never read, from its archiving; zero
	// when every object is verified within the interval.
	OldestUnverified time.Duration
	// Overdue counts the objects not read within one interval of their
	// last read, or of their archiving when never read.
	Overdue int64
}

// coveragePage is how many scans one read of a coverage answers.
const coveragePage = 1000

// Coverage computes the coverage of the storage named storage at now for a
// scrub.interval of interval (zero for never: then nothing is overdue, and
// every object ever read counts as verified), from what src records.
func Coverage(ctx context.Context, src VerificationSource, storage string, interval time.Duration, now time.Time) (StorageCoverage, error) {
	out := StorageCoverage{Storage: storage}
	after := ""
	for range maxListPages {
		page, next, err := src.ScanVerifications(ctx, storage, after, coveragePage)
		if err != nil {
			return StorageCoverage{}, err
		}
		for _, s := range page {
			out.add(s, interval, now)
		}
		if next == "" {
			out.Share = 1
			if out.Objects > 0 {
				out.Share = float64(out.Verified) / float64(out.Objects)
			}
			return out, nil
		}
		after = next
	}
	return StorageCoverage{}, fault.New(fault.Internal, "scrub: a storage's verifications did not end")
}

// add counts one scan.
func (c *StorageCoverage) add(s ScanVerification, interval time.Duration, now time.Time) {
	c.Objects += s.Objects
	within := func(t time.Time) bool { return !t.IsZero() && (interval <= 0 || !t.Before(now.Add(-interval))) }
	if within(s.Verified) {
		c.Verified += s.Objects
		return
	}
	if within(s.Presence) {
		c.PresenceOnly += s.Objects
	}
	since := s.Held
	if s.Verified.After(since) {
		since = s.Verified
	}
	if age := now.Sub(since); age > c.OldestUnverified {
		c.OldestUnverified = age
	}
	if interval > 0 && now.Sub(since) > interval {
		c.Overdue += s.Objects
	}
}

// verified records, when the supervisor is given a recorder, that the unit
// read the scan at path on the storage named storage to its end.
func (u *unitRun) verified(storage string, path label.Path, byPresence bool) outcome {
	if u.sup.cfg.Verifications == nil {
		return outcome{}
	}
	rec := u.sup.cfg.Verifications(u.p)
	if rec == nil {
		return outcome{}
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	err := rec.RecordScanVerified(ctx, ScanVerified{Fence: u.fence, Storage: storage, Path: path, ByPresence: byPresence, At: u.sup.cfg.Now()})
	if err != nil {
		return u.failure(err, reasonDatabase)
	}
	return outcome{}
}
