package api

import (
	"context"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/trial"
)

// The bounds of a trial run (profile.proto, TryProfile).
const (
	// MaxTrialFiles is the most files and records one trial run tries.
	MaxTrialFiles = 32
	// MaxTrialFileBytes bounds one file sent with a request.
	MaxTrialFileBytes = 3 << 20
	// MaxTrialSentBytes bounds the files of one request together, below
	// the 4 MiB message so the rest of the request fits.
	MaxTrialSentBytes = 3<<20 + 512<<10
	// MaxTrialHeldBytes bounds one held file a trial run reads into
	// memory.
	MaxTrialHeldBytes = trial.MaxHeldBytes
	// MaxTrialHeldTotal bounds the held files of one request together,
	// judged from their records' sizes before any is read.
	MaxTrialHeldTotal = 128 << 20
	// MaxTrialsAtOnce is the most trial runs and explanations the server
	// runs at once: each holds its files and their copies in memory.
	MaxTrialsAtOnce = 2
	// trialWait is how long a trial waits for one of those to end before
	// it is refused as exhausted.
	trialWait = 2 * time.Second
	// maxOthers is the most other matching rules an explanation answers
	// for one element.
	maxOthers = 16
	// explainAnswerBudget bounds an explanation's answer, within the 4 MiB
	// message.
	explainAnswerBudget = 3 << 20
	// MaxTrialNameBytes bounds the name a caller gives a file.
	MaxTrialNameBytes = 1 << 10
	// maxExplained is the most elements an explanation answers.
	maxExplained = 5000
	// maxAECharacters bounds an AE title (PS3.5 6.2: AE is 16 characters).
	maxAECharacters = 16
)

// trialTarget is the profile a trial run, an explanation or a check runs,
// resolved: its expanded form, the stored version it is (nil for a
// document), its checksum, and the method the engine records.
type trialTarget struct {
	expanded *v1.ExpandedProfile
	stored   *tenant.ProfileVersion
	checksum string
	method   string
	// object is the audit entry's object: "name@version", or "document"
	// and the checksum.
	object string
}

// versionMessage returns the stored version without its texts, or nil.
func (t trialTarget) versionMessage() *v1.ProfileVersion {
	if t.stored == nil {
		return nil
	}
	v := *t.stored
	v.Source, v.Expanded = nil, nil
	m, _ := versionMessage(v, false)
	return m
}

// resolveTrial resolves the profile a request names. A document with
// problems is refused with them (the caller attaches them to the error).
func resolveTrial(ctx context.Context, p tenant.Profiles, tp *v1.TrialProfile) (trialTarget, profile.Problems, error) {
	switch k := tp.GetKind().(type) {
	case *v1.TrialProfile_Version:
		name, version := k.Version.GetName(), k.Version.GetVersion()
		if err := profileName("profile.version.name", name); err != nil {
			return trialTarget{}, nil, err
		}
		if version < 0 {
			return trialTarget{}, nil, invalid("profile.version.version", "is 0 (the latest) or more")
		}
		v, err := readVersion(ctx, p, name, version)
		if err != nil {
			return trialTarget{}, nil, err
		}
		loaded, err := profile.Load(v.Name, v.Version, v.Source, v.Expanded, v.Checksum)
		if err != nil {
			return trialTarget{}, nil, fault.Wrap(fault.Internal, err, "api: a stored profile version does not load")
		}
		ref := v.Ref().String()
		return trialTarget{expanded: loaded.Expanded(), stored: &v, checksum: v.Checksum, method: ref, object: ref}, nil, nil
	case *v1.TrialProfile_Document:
		if k.Document == nil {
			return trialTarget{}, nil, invalid("profile.document", "is required")
		}
		e, ps := profile.Expand(k.Document, profile.Shipped())
		if len(ps) > 0 {
			return trialTarget{}, ps, documentRefusal("profile.document", ps)
		}
		_, sum, err := profile.Checksum(e)
		if err != nil {
			return trialTarget{}, nil, fault.Wrap(fault.Internal, err, "api: the checksum of a valid document")
		}
		// The method a file records names the document by its checksum,
		// as no version names it: an LO value of the default repertoire.
		return trialTarget{expanded: e, checksum: sum, method: "unsaved " + sum[:16], object: "document " + sum}, nil, nil
	}
	return trialTarget{}, nil, invalid("profile", "is required: a stored version or a document")
}

// trialEngine compiles the target under keys made for this request, its
// lookups never reading a table. A version this server cannot run is a
// precondition failure.
func trialEngine(t trialTarget, unassigned *int) (*engine.Engine, engine.Keys, error) {
	return trial.NewEngine(t.expanded, t.method, unassigned)
}

// enterTrial takes one of the server's places for a trial run, waiting a
// little for one to free; it returns the function that gives it back. None
// free in time is Exhausted.
func (c *core) enterTrial(ctx context.Context) (func(), error) {
	t := time.NewTimer(trialWait)
	defer t.Stop()
	select {
	case c.trials <- struct{}{}:
		return func() { <-c.trials }, nil
	case <-t.C:
		return nil, fault.New(fault.Exhausted, "as many trial runs as the server runs at once are running; try again")
	case <-ctx.Done():
		return nil, fault.New(fault.Unavailable, "the request ended before a trial could run")
	}
}

// trialInput is one file to try: its bytes and the reception it is tried
// as coming from, with the name or record it is answered under.
type trialInput struct {
	name     string
	record   string
	file     []byte
	received engine.Reception
	// receiver is the receiver whose receiving checks run; nil for none.
	receiver *tenant.Receiver
	// study is the Study Instance UID a held file's record names, as the
	// catalog would hold it; "" for a file sent with the request.
	study string
}

// trialReception checks the reception a request gives its sent files.
func trialReception(field string, r *v1.TrialReception) (engine.Reception, error) {
	out := engine.Reception{Receiver: r.GetReceiver(), CallingAETitle: r.GetCallingAeTitle(), CalledAETitle: r.GetCalledAeTitle()}
	if out.Receiver != "" && master.ValidateTenantName(out.Receiver) != nil {
		return out, invalid(field+".receiver", "is a receiver's name: 1 to 63 of a-z, 0-9 and -, starting with a letter")
	}
	for _, ae := range []struct{ name, v string }{{"calling_ae_title", out.CallingAETitle}, {"called_ae_title", out.CalledAETitle}} {
		if utf8.RuneCountInString(ae.v) > maxAECharacters || !utf8.ValidString(ae.v) {
			return out, invalid(field+"."+ae.name, "is at most 16 characters")
		}
	}
	return out, nil
}

// sentInputs checks the files a request sends.
func sentInputs(field string, files []*v1.TrialFile, received engine.Reception) ([]trialInput, error) {
	total := 0
	out := make([]trialInput, 0, len(files))
	for i, f := range files {
		at := field + "[" + strconv.Itoa(i) + "]"
		if len(f.GetName()) > MaxTrialNameBytes || !utf8.ValidString(f.GetName()) {
			return nil, invalid(at+".name", "is at most 1024 bytes of UTF-8")
		}
		if n := len(f.GetContent()); n == 0 || n > MaxTrialFileBytes {
			return nil, invalid(at+".content", "is a DICOM file of 1 byte to 3 MiB")
		}
		total += len(f.GetContent())
		if total > MaxTrialSentBytes {
			return nil, invalid(field, "the files together are at most 3.5 MiB")
		}
		out = append(out, trialInput{name: f.GetName(), file: f.GetContent(), received: received})
	}
	return out, nil
}

// heldRecords looks up the quarantine records a request names, and
// refuses before any byte is read a record that keeps no bytes, one past
// MaxTrialHeldBytes, and records together past MaxTrialHeldTotal, from
// their sizes.
func heldRecords(ctx context.Context, p tenant.Pipeline, field string, ids []string) ([]tenant.QuarantinedFile, error) {
	out := make([]tenant.QuarantinedFile, 0, len(ids))
	var total int64
	for i, id := range ids {
		at := field + "[" + strconv.Itoa(i) + "]"
		if err := serverID(at, id); err != nil {
			return nil, err
		}
		f, err := lookupFile(ctx, p, id)
		if err != nil {
			return nil, err
		}
		if !f.ContentKept {
			return nil, precondition(at + ": the record keeps no bytes of its file")
		}
		if f.Size > MaxTrialHeldBytes {
			return nil, precondition(at + ": the held file is larger than 64 MiB, the most a trial run reads")
		}
		if total += f.Size; total > MaxTrialHeldTotal {
			return nil, precondition(field + ": the held files together are larger than 128 MiB, the most one trial run reads")
		}
		out = append(out, f)
	}
	return out, nil
}

// heldInput reads the held file of a record, tried with its own
// reception's facts and, while it exists, the receiver that held it.
func (c *core) heldInput(ctx context.Context, tenantName string, p tenant.Pipeline, f tenant.QuarantinedFile) (trialInput, error) {
	b, err := c.heldFiles.HeldFile(ctx, tenantName, f, MaxTrialHeldBytes)
	if errors.Is(err, ErrNoHeldFile) {
		return trialInput{}, fault.New(fault.Unavailable, "quarantine_records: the held file of "+f.ID+" cannot be read now")
	}
	if err != nil {
		return trialInput{}, err
	}
	in := trialInput{record: f.ID, file: b, study: f.StudyUID, received: engine.Reception{
		Receiver: f.Receiver, CallingAETitle: f.Caller.CallingAETitle, CalledAETitle: f.Caller.CalledAETitle}}
	rcv, err := p.Receiver(ctx, f.Receiver)
	switch {
	case err == nil:
		in.receiver = &rcv
	case fault.KindOf(err) != fault.NotFound:
		return trialInput{}, err
	}
	return in, nil
}

// namedReceiver returns the receiver a request names for its sent files,
// nil for none; one the tenant does not have is NotFound.
func namedReceiver(ctx context.Context, p tenant.Pipeline, name string) (*tenant.Receiver, error) {
	if name == "" {
		return nil, nil
	}
	rcv, err := lookupReceiver(ctx, p, name)
	if err != nil {
		return nil, fieldOf("reception.receiver", err)
	}
	return &rcv, nil
}

// TryProfile runs a profile over sample files in memory, under keys made
// for the request, and answers each file element by element with the
// verdict a receiver would reach. Nothing is stored; its audit entry is
// the evidence of the run, with counts and no value.
func (s profileService) TryProfile(ctx context.Context, req *v1.TryProfileRequest) (*v1.TryProfileResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ProfileServiceTryProfileProcedure)
	if err != nil {
		return nil, err
	}
	pipe, st, err := c.inTenant(ctx, d, req.GetTenant(), showValuesDetails(req.GetShowValues(), nil))
	if err != nil {
		return nil, err
	}
	out, sum, object, err := c.tryProfile(ctx, req, pipe, st)
	if err != nil {
		var ps profile.Problems
		var pe problemsErr
		if errors.As(err, &pe) {
			err, ps = pe.err, pe.ps
		}
		return nil, problemsError(c.failIn(d, st, trial.SafeError(err)), ps)
	}
	trial.HideValues(out.GetResults(), req.GetShowValues())
	facts := access.Facts{Object: object, Details: showValuesDetails(req.GetShowValues(), trialDetails(sum, out.GetChecksum(), len(req.GetQuarantineRecords())))}
	entry := d.Entry(master.OutcomeDone, "", facts)
	c.log.Info("api: a profile was tried; nothing was stored", "request_id", entry.RequestID, "key_id", entry.KeyID,
		"profile", object, "files", len(out.GetResults()), "held", sum.Held)
	c.record(d, st, master.OutcomeDone, "", facts)
	return out, nil
}

// problemsErr is a document refused for its problems, carried to the
// handler that attaches them to the error.
type problemsErr struct {
	err error
	ps  profile.Problems
}

// Error is the refusal's.
func (e problemsErr) Error() string { return e.err.Error() }

// tryProfile does TryProfile's work once the tenant is in hand, and
// returns the answer, its summary and the audit entry's object.
func (c *core) tryProfile(ctx context.Context, req *v1.TryProfileRequest, pipe tenant.Pipeline, st tenant.Store) (*v1.TryProfileResponse, *trial.Summary, string, error) {
	p, err := profilesOf(st)
	if err != nil {
		return nil, nil, "", err
	}
	target, ps, err := resolveTrial(ctx, p, req.GetProfile())
	if err != nil {
		return nil, nil, "", problemsErr{err, ps}
	}
	if n := len(req.GetFiles()) + len(req.GetQuarantineRecords()); n == 0 || n > MaxTrialFiles {
		return nil, nil, "", invalid("files", "with quarantine_records, 1 to "+strconv.Itoa(MaxTrialFiles)+" files are tried at once")
	}
	received, err := trialReception("reception", req.GetReception())
	if err != nil {
		return nil, nil, "", err
	}
	inputs, err := sentInputs("files", req.GetFiles(), received)
	if err != nil {
		return nil, nil, "", err
	}
	rcv, err := namedReceiver(ctx, pipe, received.Receiver)
	if err != nil {
		return nil, nil, "", err
	}
	for i := range inputs {
		inputs[i].receiver = rcv
	}
	records, err := heldRecords(ctx, pipe, "quarantine_records", req.GetQuarantineRecords())
	if err != nil {
		return nil, nil, "", err
	}
	limit, err := c.values().HeaderLimit(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	pixelLimit, err := c.values().OnTenant(st).PixelLimit(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	leave, err := c.enterTrial(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	defer leave()
	unassigned := 0
	en, keys, err := trialEngine(target, &unassigned)
	if err != nil {
		return nil, nil, "", err
	}
	out := &v1.TryProfileResponse{Version: target.versionMessage(), Checksum: target.checksum}
	if err := engine.ForReception(target.expanded); err != nil {
		out.ReceptionRefusal = trial.ReceptionRefusal(err)
	}
	sum := trial.NewSummary()
	budget := trial.AnswerBudget
	newCheck := func(maxFileBytes int64) receive.Checker { return check.New(check.Options{MaxFileBytes: maxFileBytes}) }
	routes := &trialRouting{p: p, catalog: pipe, target: target, en: en, keys: keys, unassigned: &unassigned, limit: limit,
		routed: map[tenant.ProfileRef]routedEngine{}}
	try := func(in trialInput) error {
		unassigned = 0
		use, err := routes.of(ctx, in)
		if err != nil {
			return err
		}
		t, err := receive.TryFile(ctx, use.en, in.file, in.received, receive.TrialOptions{Keys: use.keys, HeaderLimit: limit, PixelLimit: pixelLimit, Receiver: in.receiver, Check: newCheck, Memory: c.deidMemory})
		if err != nil {
			return err
		}
		use.hold(&t)
		r := trial.Result(t, &budget)
		use.mark(r)
		r.Name, r.QuarantineRecord, r.LookupsUnassigned = in.name, in.record, int32(unassigned) //nolint:gosec // G115: at most engine.MaxLookups per file.
		sum.Add(t, r)
		out.Results = append(out.Results, r)
		return nil
	}
	for _, in := range inputs {
		if err := try(in); err != nil {
			return nil, nil, "", err
		}
	}
	for _, f := range records {
		in, err := c.heldInput(ctx, req.GetTenant(), pipe, f)
		if err != nil {
			return nil, nil, "", err
		}
		if err := try(in); err != nil {
			return nil, nil, "", err
		}
	}
	out.Summary = sum.Proto()
	return out, sum, target.object, nil
}

// ExplainProfile answers for one file which rule or base decides each
// element and why, without any value.
func (s profileService) ExplainProfile(ctx context.Context, req *v1.ExplainProfileRequest) (*v1.ExplainProfileResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ProfileServiceExplainProfileProcedure)
	if err != nil {
		return nil, err
	}
	pipe, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	out, object, err := c.explainProfile(ctx, req, pipe, st)
	if err != nil {
		var ps profile.Problems
		var pe problemsErr
		if errors.As(err, &pe) {
			err, ps = pe.err, pe.ps
		}
		return nil, problemsError(c.failIn(d, st, trial.SafeError(err)), ps)
	}
	c.record(d, st, master.OutcomeDone, "", access.Facts{Object: object})
	return out, nil
}

// explainProfile does ExplainProfile's work once the tenant is in hand.
// The answer is bounded before it is returned, so a success is recorded
// only for an answer that is sent.
func (c *core) explainProfile(ctx context.Context, req *v1.ExplainProfileRequest, pipe tenant.Pipeline, st tenant.Store) (*v1.ExplainProfileResponse, string, error) {
	p, err := profilesOf(st)
	if err != nil {
		return nil, "", err
	}
	target, ps, err := resolveTrial(ctx, p, req.GetProfile())
	if err != nil {
		return nil, "", problemsErr{err, ps}
	}
	if (req.GetFile() == nil) == (req.GetQuarantineRecord() == "") {
		return nil, "", invalid("file", "or quarantine_record: exactly one is given")
	}
	limit, err := c.values().HeaderLimit(ctx)
	if err != nil {
		return nil, "", err
	}
	var in trialInput
	var record *tenant.QuarantinedFile
	if req.GetFile() != nil {
		received, err := trialReception("reception", req.GetReception())
		if err != nil {
			return nil, "", err
		}
		inputs, err := sentInputs("file", []*v1.TrialFile{req.GetFile()}, received)
		if err != nil {
			return nil, "", fieldOf("file", err)
		}
		in = inputs[0]
	} else {
		rs, err := heldRecords(ctx, pipe, "quarantine_record", []string{req.GetQuarantineRecord()})
		if err != nil {
			return nil, "", err
		}
		record = &rs[0]
	}
	leave, err := c.enterTrial(ctx)
	if err != nil {
		return nil, "", err
	}
	defer leave()
	if record != nil {
		if in, err = c.heldInput(ctx, req.GetTenant(), pipe, *record); err != nil {
			return nil, "", err
		}
	}
	unassigned := 0
	en, _, err := trialEngine(target, &unassigned)
	if err != nil {
		return nil, "", err
	}
	explained, hold, err := receive.ExplainFile(ctx, en, in.file, in.received, in.receiver, limit)
	if err != nil {
		return nil, "", err
	}
	out := &v1.ExplainProfileResponse{Version: target.versionMessage(), Checksum: target.checksum}
	if hold != nil {
		out.Refusal = trial.HoldCause(hold)
		out.RefusalPath = hold.Details["path"]
	}
	budget := explainAnswerBudget
	for i, x := range explained {
		m := explainedMessage(x)
		size := proto.Size(m)
		if i >= maxExplained || size > budget {
			out.ElementsOmitted = int32(len(explained) - i) //nolint:gosec // G115: bounded by engine.MaxElements.
			break
		}
		budget -= size
		out.Elements = append(out.Elements, m)
	}
	return out, target.object, nil
}

// explainedMessage returns an explanation as the API answers it.
func explainedMessage(x engine.Explanation) *v1.ExplainedElement {
	e := x.Entry
	m := &v1.ExplainedElement{
		Path: e.Path, Keyword: trial.KeywordOf(e.Tag), Vr: e.VR, Action: e.Action, Rule: int32(e.Rule + 1), //nolint:gosec // G115: a position under profile.MaxExpandedRules.
		RuleName: e.RuleName, Origin: e.Origin, Reason: e.Reason, SelectedBy: x.SelectedBy,
	}
	for i, o := range x.Others {
		if i >= maxOthers {
			m.OthersOmitted = int32(len(x.Others) - maxOthers) //nolint:gosec // G115: bounded by profile.MaxExpandedRules.
			break
		}
		m.Others = append(m.Others, &v1.ExplainedCandidate{
			Rule: int32(o.Rule + 1), RuleName: o.Name, Origin: o.Origin, SelectedBy: o.SelectedBy, WhyNot: o.WhyNot, //nolint:gosec // G115: a position under profile.MaxExpandedRules.
		})
	}
	return m
}
