package testkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// FileProfile is what a synthetic instance takes from a public test file:
// its SOP class, its transfer syntax, its size and its modality. The
// catalog keeps no other property of a file but the header values a scan
// carries, which the generator draws per scan, so a tenant made from a few
// profiles has the rows a tenant of real files of those kinds would have.
type FileProfile struct {
	SOPClassUID, TransferSyntaxUID string
	Size                           int64
	// Modality is the file's Modality (0008,0060).
	Modality string
}

// ProfileOf reads the profile of file, a whole Part 10 file, with the
// pipeline's own header reader, so a profile is what reception would have
// recorded for it.
func ProfileOf(t testing.TB, file []byte) FileProfile {
	t.Helper()
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	class, _ := h.UID(header.SOPClassUID)
	modality, _ := h.Lookup(naming.NewTag(0x0008, 0x0060))
	return FileProfile{SOPClassUID: class, TransferSyntaxUID: h.TransferSyntaxUID, Size: int64(len(file)), Modality: modality}
}

// descriptions are the series descriptions a generated scan of a modality
// is given, those of research protocols; a scan of another modality, and
// one scan in twenty, has none, as files without one do.
var descriptions = map[string][]string{
	"MR": {"Localizer", "T1 MPRAGE", "T2 FLAIR", "T2 TSE axial", "DWI b1000", "ADC", "SWI", "fMRI rest run 1", "fMRI task run 2",
		"Field map", "ASL perfusion", "DTI 64 directions"},
	"CT": {"Scout", "Head 5mm", "Chest 1.25mm", "Abdomen Pelvis", "SmartScore - Gated 0.5 sec", "Angio 0.625mm", "Bone recon"},
}

// studyDate draws a study's date from r: a day in the ten years before
// the tenant starts receiving, since an archive receives old studies as
// well as new ones, or none for one study in fifty, as files without one.
func studyDate(r *rand.Rand) string {
	if r.IntN(50) == 0 {
		return ""
	}
	return genEpoch.AddDate(0, 0, -r.IntN(3653)).Format("20060102")
}

// scanValues draws the header values of scan i of a study of that date,
// from r: the profile's modality, a description of that modality and the
// series number i+1.
func scanValues(r *rand.Rand, date string, profile FileProfile, i int) tenant.ScanValues {
	description := ""
	if list := descriptions[profile.Modality]; len(list) > 0 && r.IntN(20) != 0 {
		description = list[r.IntN(len(list))]
	}
	return tenant.ScanValuesOf(date, profile.Modality, description, strconv.Itoa(i+1))
}

// TenantShape says what GenerateTenant makes.
type TenantShape struct {
	// Instances is how many instances this call writes, from study
	// FirstStudy on; its last study is cut to make the count exact.
	Instances int64
	// Seed makes the tenant: the same seed gives the same subjects,
	// sessions, scans, UIDs and checksums, on any database. Study k is a
	// function of the seed and k only, so a tenant made in parts holds the
	// studies a tenant made at once holds, but for the one each part cut.
	Seed uint64
	// FirstStudy is the index of the first study written: zero for a new
	// tenant, the NextStudy of the last call to add to one made with the
	// same seed.
	FirstStudy int
	// Profiles are the files instances are made from, chosen by scan; at
	// least one.
	Profiles []FileProfile
	// ScanInstances, when positive, is how many instances every scan holds,
	// so Instances of them make that many scans: a tenant measured by its
	// scans, as a search is. Zero draws each scan's size from the mix of
	// research series.
	ScanInstances int
	// SearchRows says each scan is committed with the row a search table
	// holds of it, drawn from the seed (SearchRowOf): without it a scan has
	// a row with no values, as a commit with no header gives.
	SearchRows bool
	// Workers is how many studies are written at once; 1 when zero.
	Workers int
	// Progress, when not nil, is called after each study is written with
	// the instances written so far by this call.
	Progress func(instances int64)
}

// GeneratedTenant counts what GenerateTenant wrote.
type GeneratedTenant struct {
	// Studies is how many studies (sessions) were written; NextStudy is
	// the index to give FirstStudy to add more.
	Studies, NextStudy int
	// Scans and Instances count the scans and instances written.
	Scans, Instances int64
}

// The receiver and storage every generated reception goes through.
const (
	GenReceiver = "synthetic"
	GenStorage  = "synthetic-store"
)

// scanSizes are the instance counts a scan is drawn from, with their
// weights: a localizer, a short series, a 3D volume of slices, a long
// series (diffusion, thin CT) and a time series of single frames. Their
// mean, about 400, and the spread from three to fifteen hundred are those
// of research MR and CT studies, where a few long series hold most of the
// instances.
var scanSizes = []struct {
	n      int
	weight int
}{{3, 20}, {30, 15}, {176, 25}, {500, 25}, {1500, 15}}

// studyPlan is one study of a generated tenant.
type studyPlan struct {
	subject int   // index of its subject
	scans   []int // instances of each scan
}

// planStudies returns the studies from index first on that hold n
// instances, the last one cut: subjects of one to four sessions (visits),
// sessions of three to twelve scans. Each study's numbers come from a
// generator seeded by the tenant's seed and the study's index only, and each
// subject's visits by the seed and the subject's index, so study k is the
// same in every call that writes it whole. The slice is indexed from study
// first.
// A positive perScan gives every scan that many instances instead of the
// mix, so a tenant of many scans is made of few instances.
func planStudies(seed uint64, first int, n int64, perScan int) []studyPlan {
	var out []studyPlan
	var total int64
	subject, visitsLeft := -1, 0
	for k := 0; total < n; k++ {
		if visitsLeft == 0 {
			subject++
			visitsLeft = 1 + rand.New(rand.NewPCG(seed, uint64(subject)<<1)).IntN(4) // #nosec G404 -- a seeded generator is the point: one seed, one tenant.
		}
		visitsLeft--
		if k < first {
			continue
		}
		r := rand.New(rand.NewPCG(seed, uint64(k)<<1|1)) // #nosec G404 -- a seeded generator is the point: one seed, one tenant.
		p := studyPlan{subject: subject}
		for range 3 + r.IntN(10) {
			w := r.IntN(100)
			size := scanSizes[len(scanSizes)-1].n
			for _, s := range scanSizes {
				if w < s.weight {
					size = s.n
					break
				}
				w -= s.weight
			}
			if perScan > 0 {
				size = perScan
			}
			if left := n - total; int64(size) > left {
				size = int(left)
			}
			p.scans = append(p.scans, size)
			total += int64(size)
			if total == n {
				break
			}
		}
		out = append(out, p)
	}
	return out
}

// GenerateTenant writes a synthetic tenant into p through the tenant port,
// as reception writes one: for each study a reception at GenReceiver that
// records its received files in batches, is identified with the labels the
// default naming rule gives, claims its instances, commits each scan in
// parts of at most tenant.MaxScanCommit, one scan after another, so a
// study's scans have ids in the order of its series and its instances are
// written scan by scan, and is archived, verified and done. Each scan
// carries its study's date (studyDate), its profile's modality, a series
// description of that modality and its series number. Labels have the
// default rule's shape (sub-, ses- and scan- with a keyed hash of the
// patient ID, study UID and series UID), and UIDs are
// UUID-derived (2.25 and a 128-bit number, PS3.5 B.2), the shape
// de-identification gives them; no real UID can be one. It adds the
// receiver and its storage when p lacks them. A failure of the store is
// returned as it is, with the study it stopped at.
func GenerateTenant(ctx context.Context, p tenant.Pipeline, shape TenantShape) (GeneratedTenant, error) {
	if shape.Instances <= 0 || len(shape.Profiles) == 0 || shape.FirstStudy < 0 {
		return GeneratedTenant{}, errors.New("a tenant shape has instances and at least one profile")
	}
	if err := ensureReceiver(ctx, p); err != nil {
		return GeneratedTenant{}, err
	}
	plan := planStudies(shape.Seed, shape.FirstStudy, shape.Instances, shape.ScanInstances)
	workers := max(shape.Workers, 1)
	g := gen{p: p, shape: shape, key: hashKey(shape.Seed)}
	var next atomic.Int64
	var out GeneratedTenant
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for {
				k := int(next.Add(1) - 1)
				if k >= len(plan) {
					return
				}
				mu.Lock()
				stop := firstErr != nil
				mu.Unlock()
				if stop || ctx.Err() != nil {
					return
				}
				n, err := g.study(ctx, shape.FirstStudy+k, plan[k])
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("study %d: %w", shape.FirstStudy+k, err)
				}
				if err == nil {
					out.Studies++
					out.Scans += int64(len(plan[k].scans))
					out.Instances += n
					if shape.Progress != nil {
						shape.Progress(out.Instances)
					}
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	out.NextStudy = shape.FirstStudy + len(plan)
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return out, firstErr
}

// ensureReceiver adds GenStorage and GenReceiver unless p has them.
func ensureReceiver(ctx context.Context, p tenant.Pipeline) error {
	audit := func(method string) master.AuditEntry {
		return master.AuditEntry{ID: hex.EncodeToString(randomBytes(16)), Time: genEpoch, LocalUser: "synthetic",
			Method: method, TenantName: "synthetic", Outcome: master.OutcomeDone}
	}
	if _, err := p.Storage(ctx, GenStorage); fault.KindOf(err) == fault.NotFound {
		s := tenant.Storage{Name: GenStorage, Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "synthetic", Path: "archive"}}
		if _, err := p.AddStorage(ctx, s, genEpoch, audit("AddStorage"), nil); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := p.Receiver(ctx, GenReceiver); fault.KindOf(err) == fault.NotFound {
		r := tenant.Receiver{
			Name: GenReceiver, Type: tenant.ReceiverDIMSE, Storage: GenStorage,
			DIMSE:       tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE"},
			QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes,
		}
		if _, err := p.AddReceiver(ctx, r, genEpoch, audit("AddReceiver"), nil); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return nil
}

// genEpoch is when a generated tenant starts receiving; study k arrives
// k minutes later.
var genEpoch = time.Date(2020, 1, 6, 8, 0, 0, 0, time.UTC)

// gen writes the studies of one tenant.
type gen struct {
	p     tenant.Pipeline
	shape TenantShape
	key   []byte // the naming rule's key
}

// hashKey is the naming key of a generated tenant, made from its seed.
func hashKey(seed uint64) []byte {
	s := sha256.Sum256(binary.BigEndian.AppendUint64([]byte("synthetic naming key"), seed))
	return s[:]
}

// uid25 returns a UUID-derived UID (PS3.5 B.2) from 16 bytes of r.
func uid25(r *rand.Rand) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], r.Uint64())
	binary.BigEndian.PutUint64(b[8:], r.Uint64())
	return "2.25." + new(big.Int).SetBytes(b[:]).String()
}

// hexOf returns n random bytes of r in hex, the shape of the pipeline's ids.
func hexOf(r *rand.Rand, n int) string {
	b := make([]byte, 0, n+8)
	for len(b) < n {
		b = binary.BigEndian.AppendUint64(b, r.Uint64())
	}
	return hex.EncodeToString(b[:n])
}

// study writes study k and returns how many instances it holds.
func (g gen) study(ctx context.Context, k int, plan studyPlan) (int64, error) {
	r := rand.New(rand.NewPCG(g.shape.Seed^0x9e3779b97f4a7c15, uint64(k))) // #nosec G404 G115 -- a seeded generator is the point, and k is a study index, never negative.
	studyUID := uid25(r)
	rid := hexOf(r, 16)
	assoc := hexOf(r, 16)
	opened := genEpoch.Add(time.Duration(k) * time.Minute)
	patientID := fmt.Sprintf("SYN%07d", plan.subject)
	subjectLabel := "sub-" + naming.HashText(g.key, []byte(patientID))
	sessionLabel := "ses-" + naming.HashText(g.key, []byte(studyUID))
	caller := tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}

	type series struct {
		uid, label string
		profile    FileProfile
		values     tenant.ScanValues
		search     search.Row
		files      []tenant.ReceivedFile
	}
	// The search rows are drawn from a generator of their own, so a tenant
	// made with them has the identifiers, labels and values of one made
	// without.
	rs := rand.New(rand.NewPCG(g.shape.Seed^0x5ea4c4, uint64(k))) // #nosec G404 G115 -- a seeded generator is the point, and k is a study index, never negative.
	all := make([]series, len(plan.scans))
	var files []tenant.ReceivedFile
	date := studyDate(r)
	for i, n := range plan.scans {
		s := series{uid: uid25(r), profile: g.shape.Profiles[r.IntN(len(g.shape.Profiles))]}
		s.label = "scan-" + naming.HashText(g.key, []byte(s.uid))
		s.values = scanValues(r, date, s.profile, i)
		for range n {
			uid := uid25(r)
			in := hexOf(r, 16)
			f := tenant.ReceivedFile{
				ReceptionID: rid, SOPInstanceUID: uid, SeriesUID: s.uid, StudyUID: studyUID,
				SOPClassUID: s.profile.SOPClassUID, TransferSyntaxUID: s.profile.TransferSyntaxUID,
				Size: s.profile.Size, SHA256: tenant.SHA256(sha256.Sum256([]byte(uid))),
				Incoming: GenReceiver + "/" + in[:2] + "/" + in, Caller: caller, Association: assoc,
			}
			s.files = append(s.files, f)
		}
		if g.shape.SearchRows {
			// The row is read from the file with the lowest SOP Instance UID.
			source := s.files[0].SOPInstanceUID
			for _, f := range s.files {
				source = min(source, f.SOPInstanceUID)
			}
			s.search = SearchRowOf(rs, source, date, i+1)
		}
		files = append(files, s.files...)
		all[i] = s
	}

	rc, _, err := g.p.OpenReception(ctx, GenReceiver, studyUID, rid, opened)
	if err != nil {
		return 0, err
	}
	last := opened.Add(time.Second)
	for start := 0; start < len(files); start += tenant.MaxReceivedBatch {
		if _, err := g.p.AddReceivedFiles(ctx, files[start:min(start+tenant.MaxReceivedBatch, len(files))], last); err != nil {
			return 0, err
		}
	}
	step := func(ev reception.Event, at time.Time) error {
		st := tenant.ReceptionStep{ID: rid, From: rc.State, Event: ev, At: at}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = subjectLabel, sessionLabel, GenStorage
		}
		var err error
		rc, err = g.p.StepReception(ctx, st, nil)
		return err
	}
	at := last.Add(tenant.DefaultQuietPeriod)
	if err := step(reception.Quiet, at); err != nil {
		return 0, err
	}
	if err := step(reception.Identify, at); err != nil {
		return 0, err
	}
	var held int64
	for _, s := range all {
		path := label.Path{Subject: subjectLabel, Session: sessionLabel, Scan: s.label}
		for start := 0; start < len(s.files); start += tenant.MaxClaimBatch {
			part := s.files[start:min(start+tenant.MaxClaimBatch, len(s.files))]
			claims := make([]tenant.InstanceClaim, len(part))
			for i, f := range part {
				claims[i] = tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
			}
			answers, err := g.p.ClaimInstances(ctx, rid, claims, at, nil)
			if err != nil {
				return 0, err
			}
			c := tenant.ScanCommit{ReceptionID: rid, StudyUID: studyUID, SeriesUID: s.uid, SubjectLabel: subjectLabel,
				SessionLabel: sessionLabel, ScanLabel: s.label, Storage: GenStorage, At: at, Values: s.values, Search: s.search,
				ManifestSHA256: tenant.SHA256(sha256.Sum256([]byte(s.uid)))}
			for i, f := range part {
				if k := answers[i].Kind; k != tenant.ClaimedNow && k != tenant.ClaimedBefore {
					return 0, fmt.Errorf("instance %d of a generated scan was answered %v: the seed repeats a tenant already there", i, k)
				}
				key, err := layout.InstanceKey(path, f.SOPInstanceUID)
				if err != nil {
					return 0, err
				}
				c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.SOPInstanceUID, Key: key,
					Length: f.Size, Size: f.Size, ArchivedSHA256: f.SHA256})
			}
			if _, err := g.p.CommitScan(ctx, c); err != nil {
				return 0, err
			}
			held += int64(len(part))
		}
	}
	for _, ev := range []reception.Event{reception.Archive, reception.Verify, reception.Finish} {
		if err := step(ev, at); err != nil {
			return 0, err
		}
	}
	return held, nil
}

// randomBytes returns n bytes from a source no seed repeats, for the ids
// of audit entries, which must differ between runs on one database.
func randomBytes(n int) []byte {
	b := make([]byte, 0, n+8)
	for len(b) < n {
		b = binary.BigEndian.AppendUint64(b, rand.Uint64()) // #nosec G404 -- an id unlike the last run's, not a secret.
	}
	return b[:n]
}
