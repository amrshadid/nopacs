package rebuild

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// pgAssert fails the test unless Postgres itself finds cond true at the
// address: the condition is judged by the server, which raises what fails
// as an error the test reports.
func pgAssert(t *testing.T, address, cond, what string) {
	t.Helper()
	testkit.PostgresExec(t, address, `DO $$ BEGIN IF NOT (`+cond+`) THEN RAISE EXCEPTION '%', '`+strings.ReplaceAll(what, "'", "''")+
		`'; END IF; END $$`)
}

// TestRebuildRefusalEndsTheJob guards a scratch schema spoiled after its
// rebuild was made: a table of the catalog's name someone else made in
// it, or a grant to another role. The worker's shadow is refused, the job
// ends failed with the refusal as its reason, not tried again until its
// attempts run out, its unit is done, and nothing that was not the job's
// is dropped.
func TestRebuildRefusalEndsTheJob(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		spoil        func(r *rig, scratch string)
	}{
		{"tables not its own", "the scratch schema holds tables that are not this rebuild's shadow", func(r *rig, scratch string) {
			testkit.PostgresExec(r.t, r.address, `CREATE TABLE `+scratch+`.subjects (x integer)`, `INSERT INTO `+scratch+`.subjects VALUES (42)`)
		}},
		{"shared with another role", "the scratch schema grants access to a role other than the tenant's", func(r *rig, scratch string) {
			role := "rbworker_" + scratch[len(scratch)-8:]
			testkit.PostgresExec(r.t, r.address, `CREATE ROLE `+role, `GRANT USAGE ON SCHEMA `+scratch+` TO `+role)
			r.t.Cleanup(func() { testkit.PostgresExec(r.t, r.address, `DROP OWNED BY `+role, `DROP ROLE `+role) })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, true)
			r.archive(studyFiles[:2]...)
			j := r.createRebuild(tenant.RebuildCompare)
			scratch := j.Rebuild.Scratch
			c.spoil(r, scratch)
			var writes atomic.Int64
			s := r.start(r.config(&writes))
			end := r.waitEnded(s, j.ID)
			if end.State != tenant.JobFailed || !strings.HasPrefix(end.Reason, c.reason) {
				t.Errorf("the rebuild ended %s (%q), want failed: %s", end.State, end.Reason, c.reason)
			}
			if w, err := r.p.Work(r.ctx, tenantUnit(j.ID)); err != nil || w.Attempt != 1 {
				t.Errorf("the refused rebuild took %d attempts (%v), want 1", w.Attempt, err)
			}
			pgAssert(t, r.address, `NOT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = '`+scratch+`' AND tablename LIKE 'rebuild%')`,
				"the job's mark is left in the scratch schema")
			if c.name == "tables not its own" {
				pgAssert(t, r.address, `(SELECT x FROM `+scratch+`.subjects) = 42`, "a table that was never the job's lost its row")
			}
		})
	}
}

// TestRebuildRecordsARefusalAsItIs guards the reason an attempt that a
// precondition stopped records: the store's own phrase (here, a scratch
// schema the operator removed), not that the database did not answer.
func TestRebuildRecordsARefusalAsItIs(t *testing.T) {
	r := newRig(t, true)
	j := r.createRebuild(tenant.RebuildCompare)
	scratch := j.Rebuild.Scratch
	testkit.PostgresExec(t, r.address, `DROP SCHEMA `+scratch+` CASCADE`)
	var writes atomic.Int64
	s := r.start(r.config(&writes))
	deadline := time.Now().Add(30 * time.Second)
	var w tenant.Work
	for {
		var err error
		if w, err = r.p.Work(r.ctx, tenantUnit(j.ID)); err == nil && w.LastError != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the unit's attempt did not fail: %+v %v", w, err)
		}
		s.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = s.Stop(ctx)
	testkit.PostgresExec(t, r.address, `CREATE SCHEMA `+scratch)
	if w.LastError != "the scratch schema does not exist" {
		t.Errorf("the failed attempt records %q, want the store's refusal", w.LastError)
	}
}

// copyScanAs writes, beside the scan at from, a scan at to of the series
// given whose manifest and metadata name the same instances (each under a
// key of to) and, before them, one instance of its own, extra: a storage
// that names an instance in two scans, the second written in more than one
// batch.
func (r *rig) copyScanAs(from, to label.Path, series, extra string) {
	r.t.Helper()
	read := func(key string) []byte {
		rc, err := r.storage.Read(r.ctx, key, 0, -1)
		if err != nil {
			r.t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		var b bytes.Buffer
		if _, err := b.ReadFrom(rc); err != nil {
			r.t.Fatal(err)
		}
		return b.Bytes()
	}
	key := func(p label.Path, uid string) string {
		k, err := layout.InstanceKey(p, uid)
		if err != nil {
			r.t.Fatal(err)
		}
		return k
	}
	mk, _ := layout.ManifestKey(from)
	m, err := manifest.Decode(bytes.NewReader(read(mk)))
	if err != nil {
		r.t.Fatal(err)
	}
	more := m.Files[0]
	more.SOPInstanceUID = extra
	m.Files = append(m.Files, more)
	for i := range m.Files {
		m.Files[i].Key = key(to, m.Files[i].SOPInstanceUID)
	}
	sk, _ := layout.MetadataKey(from)
	rd := sidecar.NewReader(bytes.NewReader(read(sk)))
	var lines []sidecar.Instance
	for {
		in, _, err := rd.Next()
		if err != nil {
			break
		}
		lines = append(lines, in)
	}
	head := *rd.Scan()
	head.SeriesUID = series
	first := lines[0]
	first.SOPInstanceUID = extra
	lines = append(lines, first)
	for i := range lines {
		lines[i].Key = key(to, lines[i].SOPInstanceUID)
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].SOPInstanceUID < lines[j].SOPInstanceUID })
	var meta bytes.Buffer
	wr := sidecar.NewWriterV3(&meta, head)
	for _, in := range lines {
		if err := wr.Add(in); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := wr.Close(); err != nil {
		r.t.Fatal(err)
	}
	enc, err := manifest.Encode(m)
	if err != nil {
		r.t.Fatal(err)
	}
	tmk, _ := layout.ManifestKey(to)
	tsk, _ := layout.MetadataKey(to)
	for k, b := range map[string][]byte{tmk: enc, tsk: meta.Bytes()} {
		if _, err := r.storage.Put(r.ctx, k, bytes.NewReader(b)); err != nil {
			r.t.Fatal(err)
		}
	}
}

// TestRebuildLeavesOutAnInstanceHeldTwice guards a storage that names one
// instance in two of its scans, the second met in a later batch of the
// second scan: every instance of a scan is asked of the shadow before its
// first write, so the scan is left out whole, tallied as one storage that
// holds an instance twice (not as two storages), and the build ends rather
// than failing every attempt on a scan written in part.
func TestRebuildLeavesOutAnInstanceHeldTwice(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	from := r.scanPaths()[0]
	sc, err := r.p.CatalogScan(r.ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	to := label.Path{Subject: from.Subject, Session: from.Session, Scan: "zzzz"}
	r.copyScanAs(from, to, sc.SeriesUID+".9", "1.1")
	var writes atomic.Int64
	j, tallies := r.run(withBatch(r.config(&writes), 1), tenant.RebuildCompare)
	if tallies[tenant.TallyLeftOutHeldElsewhere] != 1 || tallies[tenant.TallySeriesOnTwoStorages] != 0 {
		t.Errorf("the tallies are %v, want the scan that names an instance held elsewhere left out as one storage's", tallies)
	}
	if j.State != tenant.JobPartial || j.Counts.InstancesRefused != sc.Instances+1 {
		t.Errorf("the compare ended %+v, want %d instances left out", j, sc.Instances+1)
	}
	for k := range tallies {
		if strings.HasPrefix(k, "differs|") {
			t.Errorf("the scan left out made the compare differ: %v", tallies)
		}
	}
}
