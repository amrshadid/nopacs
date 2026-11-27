package api_test

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// listedTerm is a term of the test's list, which no other text holds.
const listedTerm = "Zebulonquist"

// listsDocument is the basic profile with a list of staff names, Series
// Description cleaned of its terms and a private element of a block
// LISTTEST kept.
func listsDocument() *v1.ProfileDocument {
	doc := basicDocument()
	doc.TextLists = []*v1.TextList{{Name: "staff", Version: 1, Terms: []string{listedTerm, "Ann Lee"}}}
	doc.Rules = []*v1.ProfileRule{
		{Name: "clean-series", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "SeriesDescription"}},
			Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{Lists: []string{"staff"}}}}},
		{Name: "keep-listtest", Reason: "the test keeps it",
			Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_PrivateElement{PrivateElement: &v1.PrivateElement{Creator: "LISTTEST", Group: "7FDF", Element: "10"}}},
			Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}},
	}
	return doc
}

// TestMaskedKeyIsNeverAnsweredListTerms guards the text lists' terms
// against a masked key with operator and admin on the tenant, and anything
// that would confirm a guess of them: a version with lists it reads (each
// view, and in a list of versions) has its lists named with their versions
// and no term, no expanded form and no checksum (which covers the terms),
// and says so; it may not store a version with lists (the answer for a
// document equal to a stored one would confirm it), nor try or explain a
// stored version with lists (a value changed or not tells a guessed term);
// a description and a comparison give each list's name, version and
// count, but no list's checksum and no version's; a version without lists
// is answered whole; and a key that is not masked reads it all.
func TestMaskedKeyIsNeverAnsweredListTerms(t *testing.T) {
	f := newPlaceFixture(t)
	storeVersion(t, f.env, "lists", listsDocument())
	storeVersion(t, f.env, "plain", basicDocument())
	maskedOp, _ := f.maskedKey(f.onTenant("lab1", master.RoleOperator))
	maskedAdmin, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	holds := func(m proto.Message) bool {
		b, _ := protojson.Marshal(m)
		return strings.Contains(strings.ToLower(string(b)), strings.ToLower(listedTerm))
	}
	for _, view := range []v1.ProfileView{v1.ProfileView_PROFILE_VIEW_DOCUMENT, v1.ProfileView_PROFILE_VIEW_FULL} {
		res := mustCall[*v1.GetProfileVersionResponse](t, "get", call(f.as(maskedOp).profiles.GetProfileVersion,
			&v1.GetProfileVersionRequest{Tenant: "lab1", Name: "lists", Version: 1, View: view}))
		v := res.GetVersion()
		if holds(res) || !v.GetTermsWithheld() || v.GetChecksum() != "" || v.GetExpanded() != nil ||
			len(v.GetDocument().GetTextLists()) != 1 || v.GetDocument().GetTextLists()[0].GetName() != "staff" {
			t.Errorf("a masked key's %v: %v", view, v)
		}
	}
	listed := mustCall[*v1.ListProfileVersionsResponse](t, "list", call(f.as(maskedOp).profiles.ListProfileVersions,
		&v1.ListProfileVersionsRequest{Tenant: "lab1", Name: "lists"}))
	if v := listed.GetVersions(); len(v) != 1 || v[0].GetChecksum() != "" || !v[0].GetTermsWithheld() {
		t.Errorf("a masked key's list of versions: %v", v)
	}
	plainV := mustCall[*v1.GetProfileVersionResponse](t, "get plain", call(f.as(maskedOp).profiles.GetProfileVersion,
		&v1.GetProfileVersionRequest{Tenant: "lab1", Name: "plain", Version: 1, View: v1.ProfileView_PROFILE_VIEW_FULL}))
	if v := plainV.GetVersion(); v.GetChecksum() == "" || v.GetExpanded() == nil || v.GetTermsWithheld() {
		t.Errorf("a version without lists withheld from a masked key: %v", v.GetTermsWithheld())
	}
	refused := []outcome{
		call(f.as(maskedAdmin).profiles.CreateProfileVersion, &v1.CreateProfileVersionRequest{Tenant: "lab1", Name: "lists", Document: listsDocument()}),
		call(f.as(maskedOp).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("lists", 1),
			Files: []*v1.TrialFile{{Name: "a.dcm", Content: corpusFile(t, "ct5n-2062.dcm")}}}),
		call(f.as(maskedOp).profiles.ExplainProfile, &v1.ExplainProfileRequest{Tenant: "lab1", Profile: storedVersion("lists", 1),
			File: &v1.TrialFile{Name: "a.dcm", Content: corpusFile(t, "ct5n-2062.dcm")}}),
	}
	for i, o := range refused {
		if o.err == nil || !strings.Contains(o.err.Error(), "text list") {
			t.Errorf("request %d of a masked key: %v", i, o.err)
		}
	}
	if o := call(f.as(maskedOp).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("plain", 1),
		Files: []*v1.TrialFile{{Name: "a.dcm", Content: corpusFile(t, "ct5n-2062.dcm")}}}); o.err != nil {
		t.Errorf("a masked key's trial of a version without lists: %v", o.err)
	}
	desc := mustCall[*v1.DescribeProfileResponse](t, "describe", call(f.as(maskedOp).profiles.DescribeProfile,
		&v1.DescribeProfileRequest{Tenant: "lab1", Profile: storedVersion("lists", 1)}))
	if l := desc.GetTextLists(); len(l) != 1 || l[0].GetTerms() != 2 || l[0].GetSha256() != "" || holds(desc) ||
		desc.GetChecksum() != "" || desc.GetVersion().GetChecksum() != "" {
		t.Errorf("a masked key's description: %v", desc)
	}
	next := listsDocument()
	next.TextLists[0].Terms = []string{"Ann Lee", "Bea Moss"}
	storeVersionNext(t, f.env, "lists", next)
	diff := mustCall[*v1.DiffProfileVersionsResponse](t, "diff", call(f.as(maskedOp).profiles.DiffProfileVersions,
		&v1.DiffProfileVersionsRequest{Tenant: "lab1", Name: "lists", FromVersion: 1, ToVersion: 2}))
	if l := diff.GetTextLists(); len(l) != 1 || l[0].GetFrom().GetSha256() != "" || l[0].GetTo().GetSha256() != "" ||
		diff.GetFrom().GetChecksum() != "" || diff.GetTo().GetChecksum() != "" {
		t.Errorf("a masked key's comparison: %v", diff)
	}
	plain := mustCall[*v1.GetProfileVersionResponse](t, "get", call(f.as(f.lab1Operator).profiles.GetProfileVersion,
		&v1.GetProfileVersionRequest{Tenant: "lab1", Name: "lists", Version: 1}))
	if !holds(plain) || plain.GetVersion().GetTermsWithheld() {
		t.Error("a key that is not masked was not answered the terms")
	}
	pd := mustCall[*v1.DescribeProfileResponse](t, "describe", call(f.as(f.lab1Operator).profiles.DescribeProfile,
		&v1.DescribeProfileRequest{Tenant: "lab1", Profile: storedVersion("lists", 1)}))
	if pd.GetTextLists()[0].GetSha256() == "" {
		t.Error("a key that is not masked was not answered the checksum")
	}
}

// storeVersionNext stores doc as the next version of name, as
// storeVersion stores the first.
func storeVersionNext(t *testing.T, e *env, name string, doc *v1.ProfileDocument) {
	t.Helper()
	p := e.pipelineOf("lab1").(tenant.Profiles)
	last, err := p.ProfileVersion(ctx, name, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, err := profile.New(name, last.Version+1, doc, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.CreateProfileVersion(ctx, tenant.NewProfileVersion{
		Name: name, Source: v.Source(), Expanded: v.ExpandedText(), Checksum: v.Checksum(), Note: "next"}, e.now(), e.setupEntry(), nil); err != nil {
		t.Fatal(err)
	}
}

// TestTextListsEndToEnd guards a profile's text lists through the server:
// a version with a list made by CreateProfileVersion; a file received by a
// receiver pointed at it archived with the term cut out of its Series
// Description and the rest kept; a file holding the term in an element the
// profile keeps held as residual_value, its detail naming the element and
// not the term; DescribeProfile answering each list's summary;
// DiffProfileVersions naming a list whose terms alone changed, with both
// sides' checksums; and no answer of the description or the comparison,
// and no quarantine record, holding a term.
func TestTextListsEndToEnd(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	created := mustCall[*v1.CreateProfileVersionResponse](t, "create", call(f.as(f.lab1Admin).profiles.CreateProfileVersion,
		&v1.CreateProfileVersionRequest{Tenant: "lab1", Name: "lists", Document: listsDocument(), Note: "lists"}))
	if v := created.GetVersion(); v.GetVersion() != 1 || len(v.GetChecksum()) != 64 {
		t.Fatalf("the version made: %v", v)
	}
	deid := dimseReceiver("deid1", "main", 11121)
	deid.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "lists", Version: 1}}
	mustCall[*v1.AddReceiverResponse](t, "add deid1", addReceiver("lab1", deid)(f.as(local)))

	// The corpus file's Series Description, "SmartScore - Gated 0.5 sec",
	// written over with text of its length holding the term.
	ct := corpusFile(t, "ct5n-2062.dcm")
	series := []byte("SmartScore - Gated 0.5 sec")
	if !bytes.Contains(ct, series) {
		t.Fatal("the corpus file's Series Description changed")
	}
	clean := bytes.Replace(ct, series, []byte("CT by "+listedTerm+" gated. "), 1)
	pipe, wait := receiveWithTenantKey(t, f, "deid1", clean)

	var paths []string
	_ = filepath.WalkDir(filepath.Join(f.root, "lab1", "mri"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".dcm") {
			paths = append(paths, p)
		}
		return nil
	})
	var archived [][]byte
	for _, p := range paths {
		b, err := os.ReadFile(p) // #nosec G304 -- a file of the test's own storage.
		if err != nil {
			t.Fatal(err)
		}
		archived = append(archived, b)
	}
	if len(archived) != 1 {
		t.Fatalf("%d files archived", len(archived))
	}
	if bytes.Contains(bytes.ToLower(archived[0]), bytes.ToLower([]byte(listedTerm))) || !bytes.Contains(archived[0], []byte("CT by  gated.")) {
		t.Error("the archived file holds the term, or not the rest of its Series Description")
	}

	// A second instance holding the term in the private element the
	// profile keeps.
	kept := insertBeforePixelData(t, reUID(t, clean, '7'), append(
		explicitShort(0x7FDF, 0x0010, "LO", []byte("LISTTEST")),
		explicitShort(0x7FDF, 0x1010, "LO", []byte("by "+listedTerm+" "))...))
	out := pipe.Accept(context.Background(), receive.Arrival{Receiver: "deid1", Body: bytes.NewReader(kept),
		Caller: tenant.Caller{CallingAETitle: "PRISMA1", Address: "10.20.0.31:50288"}})
	if out.Status != receive.Received {
		t.Fatalf("the second file was %v (%s)", out.Status, out.Reason)
	}
	// The clock moves as the reception helpers move it: by the quiet
	// period's steps while a reception receives, by a retry's after the
	// pipeline has been idle; until the pipeline holds the file.
	p := f.pipelineOf("lab1")
	before := failedIDs(t, p)
	idle := time.Now()
	for deadline := time.Now().Add(stallAfter); ; {
		g := wait.look(t, pipe, p, before)
		if g.ended(2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the file keeping a term was not held: %+v", g)
		}
		switch {
		case g.receiving:
			wait.clock.advance(receivingStep)
		case time.Since(idle) >= idleAfter:
			wait.clock.advance(idleStep)
			idle = time.Now()
		}
		select {
		case <-wait.woke:
		case <-time.After(settleTick):
		}
	}
	res, err := f.as(local).quarantine.ListQuarantinedFiles(context.Background(), &v1.ListQuarantinedFilesRequest{Tenant: "lab1",
		Reason: v1.QuarantineReason_QUARANTINE_REASON_RESIDUAL_VALUE, State: v1.QuarantineState_QUARANTINE_STATE_HELD})
	if err != nil || len(res.GetFiles()) != 1 {
		all, _ := f.as(local).quarantine.ListQuarantinedFiles(context.Background(), &v1.ListQuarantinedFilesRequest{Tenant: "lab1"})
		t.Fatalf("the file keeping a term was not held as residual_value: %v; held: %v", err, all.GetFiles())
	}
	held := res.GetFiles()[0]
	record := held.String()
	if d := held.GetDetails(); d["kind"] != "list_term" || d["path"] != "7FDF1010" || strings.Contains(strings.ToLower(record), strings.ToLower(listedTerm)) {
		t.Errorf("the held file's record: %s", record)
	}

	desc := mustCall[*v1.DescribeProfileResponse](t, "describe", call(f.as(f.lab1Operator).profiles.DescribeProfile,
		&v1.DescribeProfileRequest{Tenant: "lab1", Profile: storedVersion("lists", 1)}))
	if l := desc.GetTextLists(); len(l) != 1 || l[0].GetName() != "staff" || l[0].GetTerms() != 2 || len(l[0].GetSha256()) != 64 {
		t.Errorf("the description's lists: %v", l)
	}
	if strings.Contains(strings.ToLower(desc.String()), strings.ToLower(listedTerm)) {
		t.Error("the description holds a term")
	}

	// A second version whose only change is a term of the list.
	next := listsDocument()
	next.TextLists[0].Terms = []string{"Ann Lee", "Bea Moss"}
	mustCall[*v1.CreateProfileVersionResponse](t, "create 2", call(f.as(f.lab1Admin).profiles.CreateProfileVersion,
		&v1.CreateProfileVersionRequest{Tenant: "lab1", Name: "lists", Document: next}))
	diff := mustCall[*v1.DiffProfileVersionsResponse](t, "diff", call(f.as(f.lab1Operator).profiles.DiffProfileVersions,
		&v1.DiffProfileVersionsRequest{Tenant: "lab1", Name: "lists", FromVersion: 1, ToVersion: 2}))
	if l := diff.GetTextLists(); len(l) != 1 || l[0].GetName() != "staff" || l[0].GetChange() != v1.ProfileChange_PROFILE_CHANGE_CHANGED ||
		l[0].GetFrom().GetSha256() == l[0].GetTo().GetSha256() || diff.GetUnlistedChange() {
		t.Errorf("the comparison's lists: %v", diff.GetTextLists())
	}
	if s := strings.ToLower(diff.String()); strings.Contains(s, strings.ToLower(listedTerm)) || strings.Contains(s, "bea moss") {
		t.Error("the comparison holds a term")
	}
}
