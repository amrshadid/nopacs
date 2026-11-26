package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
)

// downloadError calls the download of path as c, as the server's pattern
// route does (downloadStandIn), and returns the error the handler answered,
// whose message the server writes in the answer's body.
func downloadError(t *testing.T, e *env, c caller, path string) error {
	t.Helper()
	h, err := api.New(api.Config{Access: e.access, Registry: e.registry, Master: e.faulty, NewID: e.newID, Settings: e.known, Keys: e.keys, Now: e.now})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	segs := strings.Split(path, "/")
	for i, name := range map[int]string{2: "tenant", 4: "subject", 5: "session", 6: "scan", 7: "instance"} {
		r.SetPathValue(name, segs[i])
	}
	need, facts := api.DownloadNeed(r)
	facts.Method, facts.RequestID, facts.RemoteAddress = api.DownloadPattern, "dl-relabeled", "127.0.0.1:1"
	d, err := e.access.Check(r.Context(), access.APIKey(c.key), need, facts)
	if err != nil {
		t.Fatal(err)
	}
	return h.Download(api.WithDecision(r.Context(), d), httptest.NewRecorder(), r)
}

// messageOf returns the message an API error or a handler's error carries.
func messageOf(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return fault.Message(err)
}

// isNotFound reports whether err is NOT_FOUND, as an API error or as a
// handler's error.
func isNotFound(err error) bool {
	return connect.CodeOf(err) == connect.CodeNotFound || fault.KindOf(err) == fault.NotFound
}

// labelRead is one read that names labels, made as a caller.
type labelRead struct {
	name string
	call func(who caller) error
	// want is what the pointer names for the old labels.
	want string
}

// labelReads returns every read that names labels, asked by the labels p
// and the instance uid.
func labelReads(t *testing.T, c *correctionArchive, p label.Path, uid string) []labelRead {
	ctx := context.Background()
	return []labelRead{
		{"GetSubject", func(who caller) error {
			_, err := c.as(who).catalog.GetSubject(ctx, &v1.GetSubjectRequest{Tenant: "lab1", SubjectLabel: p.Subject})
			return err
		}, "subject sub-renamed"},
		{"GetSession", func(who caller) error {
			_, err := c.as(who).catalog.GetSession(ctx, &v1.GetSessionRequest{Tenant: "lab1", SubjectLabel: p.Subject, SessionLabel: p.Session})
			return err
		}, "subject sub-renamed, session " + p.Session},
		{"GetScan", func(who caller) error {
			_, err := c.as(who).catalog.GetScan(ctx, &v1.GetScanRequest{Tenant: "lab1", SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan})
			return err
		}, "subject sub-renamed, session " + p.Session + ", scan " + p.Scan},
		{"ListSessions", func(who caller) error {
			_, err := c.as(who).catalog.ListSessions(ctx, &v1.ListSessionsRequest{Tenant: "lab1", SubjectLabel: p.Subject})
			return err
		}, "subject sub-renamed"},
		{"ListScans", func(who caller) error {
			_, err := c.as(who).catalog.ListScans(ctx, &v1.ListScansRequest{Tenant: "lab1", SubjectLabel: p.Subject, SessionLabel: p.Session})
			return err
		}, "subject sub-renamed, session " + p.Session},
		{"GetScanMetadata", func(who caller) error {
			_, err := c.as(who).metadata.GetScanMetadata(ctx, &v1.GetScanMetadataRequest{Tenant: "lab1", SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan})
			return err
		}, "scan " + p.Scan},
		{"GetInstanceMetadata", func(who caller) error {
			_, err := c.as(who).metadata.GetInstanceMetadata(ctx, &v1.GetInstanceMetadataRequest{Tenant: "lab1", SubjectLabel: p.Subject, SessionLabel: p.Session,
				ScanLabel: p.Scan, InstanceId: uid})
			return err
		}, "scan " + p.Scan},
		{"ReadFiles", func(who caller) error {
			st, err := c.as(who).data.ReadFiles(ctx, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: p.Subject})
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			_, err = st.Receive()
			return err
		}, "subject sub-renamed"},
		{"download", func(who caller) error { return downloadError(t, c.env, who, downloadPath(p, uid)) },
			"subject sub-renamed, session " + p.Session + ", scan " + p.Scan},
	}
}

// bare reports whether err is a not found that names nothing of the
// relabel id or of its new label.
func bare(err error, id string) bool {
	msg := messageOf(err)
	return isNotFound(err) && !strings.Contains(msg, "RELABELED") && !strings.Contains(msg, id) && !strings.Contains(msg, "sub-renamed")
}

// TestOldLabelsPointToTheNewOnes guards the answer to a lookup by labels a
// relabel moved: after a subject's relabel, the old label asked by an
// operator of the tenant (who may read its corrections) of every read that
// names labels (GetSubject, GetSession, GetScan, ListSessions, ListScans,
// GetScanMetadata, GetInstanceMetadata, ReadFiles and the plain download)
// is NOT_FOUND with the reason RELABELED, the relabel's id and the labels
// the data has now, and the new labels answer. A reader asking the same is
// answered a bare not found on every path, the same answer as for a label
// never relabeled, so a relabel that took an identifying label out of use
// is not undone for readers. A label no relabel moved, an instance missing
// from a scan the tenant holds, and the same old label asked of another
// tenant are bare not founds for an operator too.
func TestOldLabelsPointToTheNewOnes(t *testing.T) {
	c := newCorrectionArchive(t)
	var old label.Path
	var instance string
	for _, p := range c.a.scans(t) {
		if p.Subject == c.subject {
			old, instance = p, c.a.held(t, p)[0].SOPInstanceUID
		}
	}
	res := mustCall[*v1.RelabelResponse](t, "relabel", relabelCall(subjectTarget(c.subject), "sub-renamed")(c.as(c.lab1Admin)))
	id := res.GetCorrection().GetId()
	c.waitCorrection(t, id, func(cor *v1.Correction) bool { return cor.GetState() == v1.CorrectionState_CORRECTION_STATE_COMMITTED })
	now := label.Path{Subject: "sub-renamed", Session: old.Session, Scan: old.Scan}

	for _, r := range labelReads(t, c, old, instance) {
		err := r.call(c.lab1Operator)
		msg := messageOf(err)
		if !isNotFound(err) || !strings.HasPrefix(msg, "RELABELED: ") || !strings.Contains(msg, "the relabel "+id) || !strings.Contains(msg, r.want) {
			t.Errorf("%s by the old label, as an operator, answered %v, want NOT_FOUND RELABELED naming %s and %q", r.name, err, id, r.want)
		}
		// The reason travels as an ErrorInfo detail too, with the relabel's
		// id and the label at the depth asked (the plain download's body
		// carries no details).
		var ce *connect.Error
		if r.name != "download" {
			if !errors.As(err, &ce) {
				t.Fatalf("%s answered %v, not an API error", r.name, err)
			}
			ri, ok := codes.ReasonOfError(ce)
			if want := r.want[strings.LastIndexByte(r.want, ' ')+1:]; !ok || ri.Reason != "RELABELED" || ri.Metadata["correction"] != id || ri.Metadata["to_label"] != want {
				t.Errorf("%s by the old label: error info %+v %v, want RELABELED, %s, %s", r.name, ri, ok, id, want)
			}
		}
		if err := r.call(c.lab1Reader); !bare(err, id) {
			t.Errorf("%s by the old label, as a reader, answered %v, want a bare not found", r.name, err)
		}
	}
	for _, r := range labelReads(t, c, now, instance) {
		if err := r.call(c.lab1Reader); err != nil {
			t.Errorf("%s by the new labels: %v", r.name, err)
		}
	}
	ctx := context.Background()
	never := label.Path{Subject: "sub-never"}
	_, neverErr := c.as(c.lab1Reader).catalog.GetSubject(ctx, &v1.GetSubjectRequest{Tenant: "lab1", SubjectLabel: never.Subject})
	_, oldErr := c.as(c.lab1Reader).catalog.GetSubject(ctx, &v1.GetSubjectRequest{Tenant: "lab1", SubjectLabel: old.Subject})
	if !isNotFound(neverErr) || messageOf(oldErr) != messageOf(neverErr) {
		t.Errorf("a reader's answer for the old label (%v) differs from that for a label never relabeled (%v)", oldErr, neverErr)
	}

	others := []struct {
		name string
		err  error
	}{
		{"a label no relabel moved", func() error {
			_, err := c.as(c.lab1Operator).catalog.GetSubject(ctx, &v1.GetSubjectRequest{Tenant: "lab1", SubjectLabel: never.Subject})
			return err
		}()},
		{"an instance missing from a scan held", downloadError(t, c.env, c.lab1Operator, downloadPath(now, "2.25.1"))},
		{"the old label in another tenant", func() error {
			_, err := c.as(c.lab2Admin).catalog.GetSubject(ctx, &v1.GetSubjectRequest{Tenant: "lab2", SubjectLabel: old.Subject})
			return err
		}()},
	}
	for _, o := range others {
		if !bare(o.err, id) {
			t.Errorf("%s answered %v, want a bare not found", o.name, o.err)
		}
	}
}
