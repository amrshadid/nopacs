package api

import (
	"bytes"
	"context"
	"strconv"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// MaxExplainFile is the largest file ExplainRoute takes with the request:
// routing reads a file's header, which this bounds with room to spare.
const MaxExplainFile = 64 << 20

// The audit details of an explanation.
const (
	detailSource = "source"
	sourceFile   = "file"
)

// ExplainRoute answers what a rule set decides for one file, rule by rule,
// with the comparison that decided each and, only when asked, the values it
// saw.
func (s routeService) ExplainRoute(ctx context.Context, req *v1.ExplainRouteRequest) (*v1.ExplainRouteResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RouteServiceExplainRouteProcedure)
	if err != nil {
		return nil, err
	}
	details := explainDetails(req)
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), details)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: details}
	fail := func(err error) error { return c.failWith(d, st, facts, err) }
	if err := itemName("receiver", req.GetReceiver()); err != nil {
		return nil, fail(err)
	}
	if err := ruleVersion("version", req.GetVersion()); err != nil {
		return nil, fail(err)
	}
	rcv, err := lookupReceiver(ctx, p, req.GetReceiver())
	if err != nil {
		return nil, fail(err)
	}
	rs, err := p.RuleSet(ctx, req.GetReceiver(), req.GetVersion())
	if err != nil {
		return nil, fail(err)
	}
	var subj query.Subject
	switch src := req.GetSource().(type) {
	case *v1.ExplainRouteRequest_File:
		caller := tenant.Caller{CallingAETitle: trimAE(req.GetCallingAeTitle()), CalledAETitle: trimAE(req.GetCalledAeTitle())}
		if err := caller.Validate(); err != nil {
			return nil, fail(fieldOf("calling_ae_title", err))
		}
		if len(src.File) == 0 || len(src.File) > MaxExplainFile {
			return nil, fail(invalid("file", "is a DICOM file of 1 byte to "+strconv.Itoa(MaxExplainFile)+" bytes"))
		}
		limit, err := c.values().HeaderLimit(ctx)
		if err != nil {
			return nil, fail(err)
		}
		if subj, err = receive.RouteSubject(bytes.NewReader(src.File), int64(len(src.File)), limit, caller, c.now()); err != nil {
			return nil, fail(invalid("file", "is not a DICOM file whose header can be read"))
		}
	case *v1.ExplainRouteRequest_Instance:
		in := src.Instance
		path, err := scanPath(in.GetSubjectLabel(), in.GetSessionLabel(), in.GetScanLabel())
		if err != nil {
			return nil, fail(fieldOf("instance", err))
		}
		stores := c.newReadStores(p, req.GetTenant())
		defer stores.Close()
		h, err := c.archivedFirst(ctx, p, req.GetTenant(), path, in.GetInstanceId(), stores)
		if err != nil {
			return nil, fail(err)
		}
		if err := refuseAsReceived(d, h.held); err != nil {
			return nil, fail(err)
		}
		removed, err := removedBy(ctx, p, h.held.Profile, map[tenant.ProfileRef]func(query.Field) bool{})
		if err != nil {
			return nil, fail(err)
		}
		caller, err := sessionCaller(ctx, p, h.studyUID)
		if err != nil {
			return nil, fail(err)
		}
		subj = query.Subject{Dataset: h.ds, Catalog: tenant.RouteCatalog(caller), Removed: removed, Now: c.now()}
	default:
		return nil, fail(invalid("source", "is required: a file or an instance"))
	}
	compiled := rs.Compile()
	ev := compiled.Evaluate(subj)
	out := &v1.ExplainRouteResponse{Version: rs.Version, Decision: decisionMessage(rs, rcv, ev)}
	for _, o := range ev.Outcomes {
		out.Rules = append(out.Rules, ruleResult(o, subj, req.GetShowValues()))
		if o.Phase == tenant.PhaseForward && compiled.SeriesFiltered(o.Index) {
			out.Rules = append(out.Rules, ruleResult(compiled.Series(o.Index, subj), subj, req.GetShowValues()))
		}
	}
	c.record(d, st, master.OutcomeDone, "", facts)
	return out, nil
}

// explainDetails returns the audit details of an explanation: its source,
// "file" or the instance's path of labels (labelPath), and show_values
// when asked; never a value of the file.
func explainDetails(req *v1.ExplainRouteRequest) map[string]string {
	source := sourceFile
	if in := req.GetInstance(); in != nil {
		source = labelPath(in.GetSubjectLabel(), in.GetSessionLabel(), in.GetScanLabel())
	}
	return showValuesDetails(req.GetShowValues(), map[string]string{detailSource: source})
}

// decisionMessage returns what a rule set's evaluation decides, as the
// contract writes it.
func decisionMessage(rs tenant.RuleSet, rcv tenant.Receiver, ev tenant.Evaluation) *v1.RouteDecision {
	m := &v1.RouteDecision{ArchiveUnknown: ev.ArchiveUnknown}
	switch {
	case ev.Unusable != "":
		m.ArchiveRule, m.Unrouted = ev.Unusable, "route_unusable"
	case ev.Archive >= 0:
		a := rs.Archive[ev.Archive]
		m.ArchiveRule, m.Storage, m.Profile = a.Name, a.Storage, profileRefMessage(a.Profile)
	case ev.ArchiveUnknown:
	case rs.QuarantineUnmatched:
		m.Unrouted = "no_match"
	default:
		m.Storage = rcv.Storage
	}
	for _, i := range ev.Forward {
		m.ForwardRules = append(m.ForwardRules, rs.Forward[i].Name)
	}
	for _, i := range ev.ForwardUnknown {
		m.ForwardUnknown = append(m.ForwardUnknown, rs.Forward[i].Name)
	}
	return m
}

// ruleResult returns one rule's outcome as the contract writes it, with the
// comparison that decided it: its tag, VR and whether the file holds it,
// and the values it saw only when show is set.
func ruleResult(o tenant.RuleOutcome, s query.Subject, show bool) *v1.RuleResult {
	m := &v1.RuleResult{Phase: phaseMessage(o.Phase), Rule: o.Rule, Truth: truthMessage(o.Truth), Unusable: o.Unusable}
	if o.Unusable != "" {
		m.Truth = v1.RouteTruth_ROUTE_TRUTH_UNUSABLE
		return m
	}
	l := o.Decided
	if l == nil {
		return m
	}
	dc := &v1.DecidingComparison{Comparison: query.Print(l), Field: query.FieldString(l.Field)}
	if l.Field.Catalog == "" && len(l.Field.Path) > 0 {
		if last := l.Field.Path[len(l.Field.Path)-1]; last.Creator == "" {
			t := tag.Tag(last.Tag)
			dc.Tag = "(" + hex4(last.Tag>>16) + "," + hex4(last.Tag&0xFFFF) + ")"
			dc.Vr = t.GetVR()
		}
	}
	var values []string
	if l.Field.Catalog != "" {
		if s.Catalog != nil {
			if v, ok, err := s.Catalog(l.Field.Catalog); err == nil && ok && v != "" {
				values = []string{v}
			}
		}
	} else {
		values, _ = query.Values(s.Dataset, l.Field, s.BigEndian)
	}
	dc.Present = len(values) > 0
	if l.Field.Catalog == "" && s.Removed != nil && s.Removed(l.Field) {
		dc.Truth = v1.RouteTruth_ROUTE_TRUTH_UNKNOWN
	} else if r, err := query.Eval(l, s); err == nil {
		dc.Truth = truthMessage(r.Truth)
	}
	if show {
		dc.Values = values
	}
	m.DecidedBy = dc
	return m
}

// hex4 writes v as four upper-case hexadecimal digits.
func hex4(v uint32) string {
	s := strconv.FormatUint(uint64(v&0xFFFF), 16)
	for len(s) < 4 {
		s = "0" + s
	}
	return upperHex(s)
}

// upperHex returns s with its letters in upper case.
func upperHex(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// archivedInstance is one instance's header as the archive keeps it, in
// its scan's metadata file.
type archivedInstance struct {
	held     tenant.ScanInstance
	ds       *dataset.Dataset
	studyUID string
}

// archivedFirst reads the header of the instance id of the scan at path,
// or of its first instance by SOP Instance UID for id "", from the scan's
// metadata file: one bounded read of one line. A scan or instance the
// tenant does not hold is NotFound; a metadata file missing or damaged, or
// one that does not hold the instance's header, is data loss.
func (c *core) archivedFirst(ctx context.Context, p tenant.Pipeline, tenantName string, path label.Path, id string, stores *checkedread.Stores) (archivedInstance, error) {
	scan, err := lookupScan(ctx, p, path)
	if err != nil {
		return archivedInstance{}, err
	}
	var held tenant.ScanInstance
	if id != "" {
		if err := tenant.ValidateInstanceUID(id); err != nil {
			return archivedInstance{}, fieldOf("instance.instance_id", err)
		}
		if held, err = p.HeldInstance(ctx, path, id); fault.KindOf(err) == fault.NotFound {
			err = fault.Wrap(fault.NotFound, err, messageNoInstance)
		}
		if err != nil {
			return archivedInstance{}, err
		}
	} else {
		first, _, err := p.ListHeldInstances(ctx, path, master.Page{Limit: 1})
		if err != nil {
			return archivedInstance{}, err
		}
		if len(first) == 0 {
			return archivedInstance{}, fault.New(fault.NotFound, messageNoInstance)
		}
		held = first[0]
	}
	src, _, err := c.metadataSource(ctx, p, tenantName, scan, path, stores)
	if err != nil {
		return archivedInstance{}, err
	}
	rd, rc, line, err := src.lineOf(ctx, held.SOPInstanceUID, nil)
	if err != nil {
		return archivedInstance{}, err
	}
	_ = rc.Close()
	if line.DatasetOmitted || len(line.Dataset) == 0 {
		return archivedInstance{}, fault.New(fault.DataLoss, "the instance's header is not in its scan's metadata file")
	}
	ds := dataset.NewDataset()
	if err := ds.FromDICOMJSONString(string(line.Dataset)); err != nil {
		return archivedInstance{}, fault.New(fault.DataLoss, messageBadSidecar)
	}
	out := archivedInstance{held: held, ds: ds}
	if h := rd.Scan(); h != nil {
		out.studyUID = h.StudyUID
	}
	return out, nil
}

// sessionCaller returns the AE titles the session of the study came with,
// as its session keeps them; none for a study the catalog does not have.
func sessionCaller(ctx context.Context, p tenant.Pipeline, studyUID string) (tenant.Caller, error) {
	if studyUID == "" {
		return tenant.Caller{}, nil
	}
	labels, err := p.CatalogLabels(ctx, studyUID, nil)
	if err != nil || labels.Session == nil {
		return tenant.Caller{}, err
	}
	r := labels.Session.Route
	return tenant.Caller{CallingAETitle: r.CallingAE, CalledAETitle: r.CalledAE}, nil
}

// removedBy returns, for an archived header de-identified by the profile
// version ref, whether the profile may have removed or replaced the
// attribute a field names (query.Subject.Removed): when any of its rules
// whose selector selects the attribute acts on it otherwise than keep,
// whatever its conditions, or when none does and the version's default
// removes it (a private element, or every element no rule names). It errs
// toward unknown, so a dry run never claims what it cannot know. The zero
// ref, a header archived as received, removes nothing: nil. cache holds
// what a call made, by version.
func removedBy(ctx context.Context, p tenant.Pipeline, ref tenant.ProfileRef, cache map[tenant.ProfileRef]func(query.Field) bool) (func(query.Field) bool, error) {
	if ref.IsZero() {
		return nil, nil
	}
	if f, ok := cache[ref]; ok {
		return f, nil
	}
	v, err := loadedProfile(ctx, p, "profile", ref)
	if err != nil {
		return nil, err
	}
	expanded := v.Expanded()
	prog, err := profile.Compile(expanded)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the stored profile version "+ref.String()+" does not compile")
	}
	rules := expanded.GetRules()
	f := func(fd query.Field) bool {
		if len(fd.Path) == 0 {
			return false
		}
		last := fd.Path[len(fd.Path)-1]
		loc := profile.Location{Tag: last.Tag, Creator: last.Creator, VR: tag.Tag(last.Tag).GetVR()}
		if last.Creator != "" {
			// A private element by its creator, in the first block a
			// creator may reserve (PS3.5 7.8.1): its selectors read the
			// creator, the group and the element's low byte.
			loc.Tag = last.Tag&0xFFFF0000 | 0x1000 | last.Tag&0xFF
			loc.VR = "UN"
		}
		for _, s := range fd.Path[:len(fd.Path)-1] {
			loc.Sequences = append(loc.Sequences, s.Tag)
		}
		matches := prog.Matches(loc, noFacts{})
		for _, m := range matches {
			if m.Index < len(rules) && rules[m.Index].GetRule().GetAction().GetKeep() == nil {
				return true
			}
		}
		if len(matches) > 0 {
			return false
		}
		return prog.Decide(loc, noFacts{}).Action.GetKeep() == nil
	}
	cache[ref] = f
	return f, nil
}

// noFacts is the facts of a judgment made from a profile's selectors
// alone: every subject absent.
type noFacts struct{}

// Value implements profile.Facts.
func (noFacts) Value(v1.ConditionSubject) (string, bool) { return "", false }
