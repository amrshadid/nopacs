package storageswitch

import (
	"context"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The blockers of verification, from the contract's fixed set of phrases
// (SwitchStorageResponse.blockers): the checks of the replica a person runs
// before a switch, read from the jobs, which skip_verification with its
// reason passes over.
const (
	// BlockerNoCleanScrub is a replica with no scrub of it done since its
	// backfill ended that found nothing.
	BlockerNoCleanScrub = "no clean scrub of the replica since its backfill"
	// BlockerNoEmptyCompare is a replica with no rebuild compare built from
	// it alone, done since its backfill ended, that found no difference.
	BlockerNoEmptyCompare = "no empty compare from the replica"
)

// Jobs reads the tenant's jobs and what each counted (tenant.Sending).
type Jobs interface {
	// ListJobs returns a page of the jobs the filter keeps, newest first.
	ListJobs(ctx context.Context, filter tenant.JobFilter, page master.Page) ([]tenant.Job, string, error)
	// JobTallies returns what a job counted, by key.
	JobTallies(ctx context.Context, id string) ([]tenant.JobTally, error)
}

// CompareFrom answers the one storage a rebuild compare job built its
// shadow from, as if the tenant had no other (RebuildJob.from_storage), or
// "" for a compare of every storage.
type CompareFrom func(j tenant.Job) string

// maxJobPages bounds the pages of jobs a check of verification reads: the
// jobs done since a backfill ended, newest first, of which the newest
// scrub of the replica and the newest compare from it are wanted.
const maxJobPages = 20

// verification returns the blockers of verification that hold for a switch
// to replica whose backfill ended at since (zero while one is owed): each
// unless the newest done job of its kind, made at or after since, counted
// nothing wrong. Only the newest decides, clean or not: a later scrub that
// found damage, or a later compare that found a difference, says the
// replica is not what an older clean check saw, so the older one lifts
// nothing; a job still running counts neither way. A compare is told from
// the replica alone only through from; nil, no compare is, and its blocker
// always holds.
func verification(ctx context.Context, jobs Jobs, from CompareFrom, replica string, since time.Time) ([]string, error) {
	if since.IsZero() {
		return []string{BlockerNoCleanScrub, BlockerNoEmptyCompare}, nil
	}
	scrubbed, compared := false, false
	scrubSeen, compareSeen := false, from == nil
	page := master.Page{Limit: master.MaxPageLimit}
	for range maxJobPages {
		// Newest first: the first job of each kind met is the one that
		// decides.
		list, next, err := jobs.ListJobs(ctx, tenant.JobFilter{States: []tenant.JobState{tenant.JobDone}, Since: since}, page)
		if err != nil {
			return nil, err
		}
		for _, j := range list {
			switch {
			case !scrubSeen && j.Kind == tenant.JobScrub && j.Storage == replica:
				scrubSeen = true
				if scrubbed, err = clean(ctx, jobs, j.ID, "finding|"); err != nil {
					return nil, err
				}
			case !compareSeen && j.Kind == tenant.JobRebuild && j.Rebuild != nil &&
				j.Rebuild.Mode == tenant.RebuildCompare && from(j) == replica:
				compareSeen = true
				if compared, err = clean(ctx, jobs, j.ID, "differs|"); err != nil {
					return nil, err
				}
			}
			if scrubSeen && compareSeen {
				return blockersOf(scrubbed, compared), nil
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	return blockersOf(scrubbed, compared), nil
}

// blockersOf returns the blockers of verification a clean scrub and an
// empty compare, each found or not, leave, in the contract's order.
func blockersOf(scrubbed, compared bool) []string {
	var out []string
	if !scrubbed {
		out = append(out, BlockerNoCleanScrub)
	}
	if !compared {
		out = append(out, BlockerNoEmptyCompare)
	}
	return out
}

// clean reports whether the job of id counted nothing under a key that
// starts with prefix: a scrub's findings (tenant.ScrubTally), a compare's
// differences (tenant.DifferTally).
func clean(ctx context.Context, jobs Jobs, id, prefix string) (bool, error) {
	tallies, err := jobs.JobTallies(ctx, id)
	if err != nil {
		return false, err
	}
	for _, t := range tallies {
		if strings.HasPrefix(t.Key, prefix) && t.Count > 0 {
			return false, nil
		}
	}
	return true, nil
}
