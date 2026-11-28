package deidjob

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Change control: before a receiver is pointed at a new profile version, the
// version is tried over what the receiver last received, as the preview of a
// re-run judges archived files (the same judge, the same tightening and the
// same check after), so the trial and the re-run never disagree on what a
// version does to a file.

// MaxTrialScans bounds the scans one trial reads a file of: a trial is
// answered within one request, so it reads the first instance of each scan,
// not every instance.
const MaxTrialScans = 2000

// ChangeTrial asks what a profile version would do to what a receiver last
// received.
type ChangeTrial struct {
	// Receiver names the receiver; Profile the version; Sessions how many of
	// the receiver's newest sessions (deid.change-trial-sessions).
	Receiver string
	Profile  tenant.ProfileRef
	Sessions int
}

// TrialElement is one element a trial found the version changes: its tag as
// eight hexadecimal digits, the version's action and how many files it
// changes it in. It carries no value.
type TrialElement struct {
	Tag, Action string
	Files       int64
}

// TrialHold is a reason the version would hold files, "reason/cause", and
// how many.
type TrialHold struct {
	Reason string
	Files  int64
}

// ChangeTrialResult is what a trial found, as counts and fixed words.
type ChangeTrialResult struct {
	// Sessions and Files are what was read: the sessions found and a file of
	// each of their scans.
	Sessions, Files int64
	// Changed are the files the version would give a new revision; Held the
	// files it would not write (its check after holds them, it needs the
	// original, it loosens what made them, a pixel rule); Unchanged those it
	// changes nothing in; Skipped those not judged (archived as received,
	// too large, missing).
	Changed, Held, Unchanged, Skipped int64
	// Elements are what changes, most files first; Holds why files are held.
	Elements []TrialElement
	Holds    []TrialHold
}

// TryChange runs the version over the first file of each scan of the
// receiver's newest sessions, changing nothing, and returns the counts. An
// error is a cause that is not a file's: the tenant not ready, a database or
// a storage that does not answer, the call's own end.
func (s *Supervisor) TryChange(ctx context.Context, tenantName string, t ChangeTrial) (ChangeTrialResult, error) {
	var res ChangeTrialResult
	st, err := s.cfg.Tenants.Store(tenantName)
	if err != nil {
		return res, err
	}
	p, ok := st.(tenant.Pipeline)
	reads, rok := st.(tenant.CatalogReads)
	if !ok || !rok {
		return res, fault.New(fault.Internal, "deidjob: the tenant's database keeps no catalog to try a version over")
	}
	u := &unitRun{t: &tenantRun{sup: s, name: tenantName}, sup: s, ctx: ctx, log: slog.New(slog.DiscardHandler),
		bufs: [2][]byte{make([]byte, readBuffer), make([]byte, readBuffer)}}
	_, pixelLimit, headerLimit, err := u.settings(st)
	if err != nil {
		return res, err
	}
	makeKeys := throwawayKeys
	if s.cfg.seams.keys != nil {
		makeKeys = s.cfg.seams.keys
	}
	keys, err := makeKeys()
	if err != nil {
		return res, err
	}
	defer func() {
		for _, k := range [][]byte{keys.UID, keys.Date, keys.Hash} {
			clear(k)
		}
	}()
	u.stores = s.cfg.OpenStorages(tenantName, p)
	defer u.stores.Close()
	j := &judge{mode: tenant.DeidRerunPreview, profile: t.Profile, p: p, keys: keys, engines: map[tenant.ProfileRef]*engine.Engine{},
		rules: map[tenant.ProfileRef][]*v1.ExpandedRule{}, receptions: map[string]receptionFacts{}, unusable: map[tenant.ProfileRef]bool{},
		headerLimit: headerLimit, pixelLimit: min(pixelLimit, MaxPixelBytes), memory: s.cfg.Memory, expanded: expandedOf{},
		rr: &rerunState{loosening: map[[2]tenant.ProfileRef]string{}}}
	sessions, err := reads.ListSessions(ctx, tenant.SessionFilter{Receiver: t.Receiver, Order: tenant.OrderNewest},
		master.Page{Limit: t.Sessions})
	if err != nil {
		return res, err
	}
	tallies := map[string]int64{}
	scans := 0
	for _, ses := range sessions.Items {
		res.Sessions++
		page := master.Page{Limit: master.MaxPageLimit}
		for {
			sc, err := reads.ListScans(ctx, tenant.ScanFilter{SubjectLabel: ses.SubjectLabel, SessionLabel: ses.Label, Order: tenant.OrderLabel}, page)
			if err != nil {
				return res, err
			}
			for _, scan := range sc.Items {
				if scans++; scans > MaxTrialScans {
					return res, fault.New(fault.Precondition, "the receiver's newest sessions hold more scans than one trial reads; lower deid.change-trial-sessions")
				}
				path := label.Path{Subject: ses.SubjectLabel, Session: ses.Label, Scan: scan.Label}
				if err := s.tryScan(ctx, u, j, reads, path, &res, tallies); err != nil {
					return res, err
				}
			}
			if sc.Next == "" {
				break
			}
			page.Cursor = sc.Next
		}
	}
	for k, n := range tallies {
		parts := strings.Split(k, "|")
		switch {
		case len(parts) == 4 && parts[0] == tallyRerun && parts[1] == "element":
			res.Elements = append(res.Elements, TrialElement{Tag: parts[2], Action: parts[3], Files: n})
		case len(parts) >= 2 && parts[0] == tallyHold:
			res.Holds = append(res.Holds, TrialHold{Reason: strings.Join(parts[1:], "/"), Files: n})
		}
	}
	sort.Slice(res.Elements, func(a, b int) bool {
		x, y := res.Elements[a], res.Elements[b]
		if x.Files != y.Files {
			return x.Files > y.Files
		}
		return x.Tag+x.Action < y.Tag+y.Action
	})
	sort.Slice(res.Holds, func(a, b int) bool {
		return res.Holds[a].Files > res.Holds[b].Files || res.Holds[a].Files == res.Holds[b].Files && res.Holds[a].Reason < res.Holds[b].Reason
	})
	return res, nil
}

// tryScan judges the first instance of one scan, adding what it found to
// res and tallies.
func (s *Supervisor) tryScan(ctx context.Context, u *unitRun, j *judge, reads tenant.CatalogReads, path label.Path, res *ChangeTrialResult,
	tallies map[string]int64) error {
	ins, _, err := reads.ListHeldInstances(ctx, path, master.Page{Limit: 1})
	if err != nil || len(ins) == 0 {
		return err
	}
	in := ins[0]
	u.scan, j.subject = path, path.Subject
	res.Files++
	file, err := u.read(in)
	switch {
	case errors.Is(err, errTooLarge), errors.Is(err, errMissing), checkedread.Mismatched(err):
		res.Skipped++
		return nil
	case err != nil:
		return err
	}
	v, err := j.judgeFile(ctx, in, file)
	if err != nil {
		return err
	}
	for k, n := range v.tallies {
		tallies[k] += n
	}
	switch {
	case v.kind == resultRefused:
		res.Held++
	case v.tallies[tallyKey(tallyRerun, "would_change")] > 0:
		res.Changed++
	case v.tallies[tallyKey(tallyRerun, "unchanged")] > 0:
		res.Unchanged++
	default:
		res.Skipped++
	}
	return nil
}
