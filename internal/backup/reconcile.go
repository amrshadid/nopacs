package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/rebuild"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The reconcile of one restored tenant with its storages: the storages
// taken over, the correction records adopted, the catalog rebuilt.

// maxListPages bounds the pages of one listing of correction records.
const maxListPages = 1 << 20

// maxDisagreements bounds the ids of records that disagree with the backup
// a report names; the count is always whole.
const maxDisagreements = 20

// reconcile marks the tenant's quarantined files whose bytes are gone as
// incoming lost, takes the tenant's storages over (their owner records one
// generation past both the record found and the database's), adopts every
// correction record they hold (tenant.Restoring), then rebuilds the catalog
// from them through a rebuild job of replace mode, which begins a new event
// history.
func reconcile(ctx context.Context, cfg RestoreConfig, m master.Store, installation string, t openedTenant, now time.Time) (TenantResult, error) {
	res := TenantResult{Tenant: t.row.Name}
	p, ok := t.store.(tenant.Pipeline)
	rs, ok2 := t.store.(tenant.Restoring)
	if !ok || !ok2 {
		return res, fault.New(fault.Internal, "backup: this database type cannot be reconciled")
	}
	var err error
	if res.QuarantineMarked, err = rs.MarkQuarantineLost(ctx); err != nil {
		return res, err
	}
	opener := place.Opener{Values: settingvalue.Reader{Master: m}, Keys: cfg.Keys, StaleUploadAge: -1}
	pt := place.TenantOf(t.row.ID, t.row.Name, t.store)
	held, err := rs.StorageOwners(ctx)
	if err != nil {
		return res, err
	}
	generations := map[string]int64{}
	for _, o := range held {
		generations[o.Storage] = o.Generation
	}
	storages, err := listStorages(ctx, p)
	if err != nil {
		return res, err
	}
	for _, s := range storages {
		st, err := opener.Open(ctx, pt, s)
		if err != nil {
			return res, fault.Wrap(fault.KindOf(err), err, "the storage "+s.Name+" cannot be opened: "+fault.Message(err))
		}
		gen, err := takeOwner(ctx, st, ownerClaim{installation: installation, tenantID: t.row.ID, storage: s.Name,
			held: generations[s.Name], takeOver: cfg.TakeOver, keys: cfg.Keys}, now)
		if err == nil {
			err = rs.SetStorageOwner(ctx, s.Name, gen, now)
		}
		if err == nil {
			res.Owners = append(res.Owners, Owner{Storage: s.Name, Generation: gen})
			err = adoptRecords(ctx, st, rs, recordAuth{tenantID: t.row.ID, keys: cfg.Keys}, s.Name, now, &res)
		}
		_ = st.Close()
		if err != nil {
			return res, err
		}
	}
	job, tallies, err := rebuildCatalog(ctx, cfg, m, t, p, opener, now)
	res.Rebuild, res.Tallies = job, tallies
	if err != nil {
		return res, err
	}
	res.Relinked, err = rs.RelinkHidden(ctx)
	return res, err
}

// listStorages returns every storage of the tenant.
func listStorages(ctx context.Context, p tenant.Pipeline) ([]tenant.Storage, error) {
	var out []tenant.Storage
	cursor := ""
	for {
		page, next, err := p.ListStorages(ctx, master.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// ownerClaim is what a restore asks of one storage's owner record.
type ownerClaim struct {
	// installation and tenantID are who the restore writes as the owner.
	installation, tenantID string
	// storage is the storage's name, for the refusal.
	storage string
	// held is the generation the backup's database holds for it.
	held int64
	// takeOver lets a record naming another owner, or a later generation,
	// be written over (RestoreConfig.TakeOver).
	takeOver bool
	// keys authenticates the record written, and checks the one found when
	// it names this installation.
	keys *secret.Keys
	// from is the installation the record found is expected to name: this
	// one for a restore, the exporting one for an import; "" is
	// installation. A record that names from but another installation's
	// is read as it is, since only its writer can check its MAC: the
	// export's own file, authenticated by the transfer key, says which
	// owner and generation it held.
	from string
	// what names the file the held generation comes from, for the refusal:
	// "the backup" or "the export".
	what string
	// again says how to run the command again to take the storage over.
	again string
}

// takeOwner writes the storage's owner record naming this installation, at
// one generation past both the record there and the one held, and returns
// it. A record there that names another installation or tenant than the
// claim expects, or a generation past the one held, says someone took the
// storage over after the file was made (another installation it was handed
// to, or a restore since, which may still be writing to it); so does a
// record naming this installation whose MAC does not verify under its key
// file, which this installation did not write. Either is refused unless the
// claim says to take it over, since two installations writing one storage
// each hide the other's writes.
func takeOwner(ctx context.Context, st blob.Store, c ownerClaim, now time.Time) (int64, error) {
	gen := c.held
	from, what, again := c.from, c.what, c.again
	if from == "" {
		from = c.installation
	}
	if what == "" {
		what = "the backup"
	}
	if again == "" {
		again = "run the restore again with --take-over, so this installation owns it (a restore interrupted after it took the storage " +
			"over is run again the same way)"
	}
	rc, err := st.Read(ctx, layout.OwnerKey, 0, -1)
	switch {
	case err == nil:
		o, derr := record.DecodeOwner(rc)
		_ = rc.Close()
		if derr != nil && !c.takeOver {
			return 0, fault.Wrap(fault.Invalid, derr, "the storage "+c.storage+"'s owner record cannot be read; "+again)
		}
		if derr == nil {
			forged := ""
			if o.Installation == c.installation && record.VerifyOwner(o, c.keys) != nil {
				forged = " (its MAC does not verify under this key file: this installation did not write it)"
			}
			if !c.takeOver && (o.Installation != from || o.Tenant != c.tenantID || o.Generation > c.held || forged != "") {
				return 0, fault.New(fault.Conflict, "the storage "+c.storage+"'s owner record names installation "+output.Clean(o.Installation)+
					", tenant "+output.Clean(o.Tenant)+", at owner generation "+strconv.FormatInt(o.Generation, 10)+forged+", and "+what+" holds generation "+
					strconv.FormatInt(c.held, 10)+" for installation "+from+", tenant "+c.tenantID+
					": another installation, or a restore made since, took it over. Stop whatever uses it now, then "+again)
			}
			gen = max(gen, o.Generation)
		}
	case fault.KindOf(err) != fault.NotFound:
		return 0, err
	}
	gen, err = nextGeneration(c.storage, gen)
	if err != nil {
		return 0, err
	}
	if err := writeOwner(ctx, st, record.Owner{Installation: c.installation, Tenant: c.tenantID, Generation: gen, Written: now}, c.keys); err != nil {
		return 0, err
	}
	return gen, nil
}

// nextGeneration returns the owner generation after gen, refused past
// record.MaxGeneration.
func nextGeneration(storage string, gen int64) (int64, error) {
	gen++
	if gen > record.MaxGeneration {
		return 0, fault.New(fault.Invalid, "the storage "+storage+"'s owner generation cannot grow past "+strconv.FormatInt(record.MaxGeneration, 10))
	}
	return gen, nil
}

// writeOwner writes o, signed with keys, as the storage's owner record.
func writeOwner(ctx context.Context, st blob.Store, o record.Owner, keys *secret.Keys) error {
	o.Format = record.Format
	b, err := record.EncodeOwner(o, keys)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "the owner record cannot be made")
	}
	_, err = st.Put(ctx, layout.OwnerKey, bytes.NewReader(b))
	return err
}

// recordAuth is what a correction record is checked against: the tenant's
// id and the installation keys (record.DecodeCorrection).
type recordAuth struct {
	tenantID string
	keys     record.Keyring
}

// adoptRecords adopts every correction record of the storage that verifies
// under the key file for the tenant. One whose MAC is missing or does not
// verify was not written by this installation for this tenant, or was
// changed since: anyone who can write to a storage can put one there, so it
// is counted and named in the report and treated as if it were not there.
// The rules for a record that verifies are unchanged: it alone still never
// decides what its row says (tenant.Restoring.AdoptCorrection).
func adoptRecords(ctx context.Context, st blob.Store, rs tenant.Restoring, auth recordAuth, storage string, now time.Time, res *TenantResult) error {
	token := ""
	for range maxListPages {
		page, err := st.List(ctx, blob.ListOptions{Prefix: layout.CorrectionsPrefix, Token: token})
		if err != nil {
			return err
		}
		for _, key := range page.Keys {
			id, err := layout.ParseCorrectionKey(key)
			if err != nil {
				return fault.New(fault.Invalid, "a key under the correction records of "+storage+" is not a record's")
			}
			rc, err := st.Read(ctx, key, 0, -1)
			if err != nil {
				return err
			}
			rec, err := record.DecodeCorrection(rc, auth.tenantID, auth.keys)
			_ = rc.Close()
			if errors.Is(err, record.ErrUnauthenticated) {
				res.Records++
				res.Unauthenticated++
				if len(res.UnauthenticatedIDs) < maxDisagreements {
					res.UnauthenticatedIDs = append(res.UnauthenticatedIDs, id)
				}
				continue
			}
			if err != nil {
				return fault.Wrap(fault.Invalid, err, "a correction record of "+storage+" cannot be read")
			}
			got, err := rs.AdoptCorrection(ctx, rec, storage, now)
			if err != nil {
				return err
			}
			res.Records++
			switch got {
			case tenant.AdoptedAdded:
				res.Added++
			case tenant.AdoptedDisagrees:
				res.Disagreed++
				if len(res.Disagreements) < maxDisagreements {
					res.Disagreements = append(res.Disagreements, rec.ID)
				}
			case tenant.AdoptedScans:
				res.Scans++
			}
		}
		if token = page.Next; token == "" {
			return nil
		}
	}
	return fault.New(fault.Unavailable, "the listing of the correction records did not end")
}

// oneTenant is the installation as the rebuild supervisor sees it during a
// restore: the one tenant being reconciled.
type oneTenant struct {
	name  string
	store tenant.Store
}

// Names implements rebuild.Tenants.
func (o oneTenant) Names(context.Context) ([]string, error) { return []string{o.name}, nil }

// Store implements rebuild.Tenants.
func (o oneTenant) Store(string) (tenant.Store, error) { return o.store, nil }

// rebuildPoll is how often a restore looks at its rebuild job.
const rebuildPoll = 100 * time.Millisecond

// rebuildCatalog makes the tenant's catalog again from its storages: a
// rebuild job of replace mode, worked here by the rebuild supervisor alone
// (nothing else runs during a restore, so nothing lands that the shadow
// would miss), until it ends. A job that ends without making its shadow
// the catalog in use fails the restore.
func rebuildCatalog(ctx context.Context, cfg RestoreConfig, m master.Store, t openedTenant, p tenant.Pipeline, opener place.Opener,
	now time.Time) (tenant.Job, []tenant.JobTally, error) {
	rj := &tenant.RebuildJob{Mode: tenant.RebuildReplace}
	if t.row.Driver == master.PostgresDriver {
		if cfg.Scratch == "" {
			return tenant.Job{}, nil, fault.New(fault.Invalid, "the tenant is on Postgres: name the scratch schema its catalog is rebuilt in (--scratch-schema)")
		}
		rj.Scratch = cfg.Scratch
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return tenant.Job{}, nil, fault.Wrap(fault.Internal, err, "no random id could be made")
	}
	method, user, after := restoreMethod, "restore", "a restore"
	if cfg.method != "" {
		method, user, after = cfg.method, cfg.localUser, "an import"
	}
	job, err := p.CreateJob(ctx, tenant.NewJob{ID: user + hex.EncodeToString(id), Kind: tenant.JobRebuild, Rebuild: rj, Created: now},
		master.AuditEntry{ID: server.NewID(), Time: now, LocalUser: user, Method: method, TenantName: t.row.Name,
			Outcome: master.OutcomeDone, Reason: "the catalog rebuilt from the storages after " + after}, nil)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	tenantID := func(context.Context, string) (string, error) { return t.row.ID, nil }
	sup, err := rebuild.New(rebuild.Config{
		Tenants: oneTenant{name: t.row.Name, store: t.store},
		OpenStorages: func(name string, p tenant.Pipeline) rebuild.Storages {
			return checkedread.NewStores(opener, tenantID, p, name)
		},
		PollInterval: rebuildPoll,
		Logger:       cfg.Log,
		// A record that verifies was written by this installation, but may
		// be older or newer than the backup; after the adoption every record
		// has a row, so the row decides.
		Decide:   rebuild.ByRow,
		Keys:     cfg.Keys,
		TenantID: tenantID,
	})
	if err != nil {
		return job, nil, err
	}
	if err := sup.Start(ctx); err != nil {
		return job, nil, err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = sup.Stop(sctx)
	}()
	deadline := time.Now().Add(cfg.RebuildTimeout)
	for {
		job, err = p.Job(ctx, job.ID)
		if err != nil {
			return job, nil, err
		}
		if job.State.Ended() {
			break
		}
		if time.Now().After(deadline) {
			return job, nil, fault.New(fault.Unavailable, "the rebuild of the catalog did not end in time")
		}
		sup.Poke(t.row.Name)
		select {
		case <-ctx.Done():
			return job, nil, fault.Wrap(fault.Unavailable, ctx.Err(), "the restore was stopped during the rebuild")
		case <-time.After(rebuildPoll):
		}
	}
	tallies, err := p.JobTallies(ctx, job.ID)
	if err != nil {
		return job, nil, err
	}
	if job.Rebuild == nil || !job.Rebuild.Activated {
		return job, tallies, fault.New(fault.Unavailable, "the rebuild of the catalog ended "+string(job.State)+" without making its catalog the one in use: "+job.Reason)
	}
	return job, tallies, nil
}
