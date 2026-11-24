package tui

import (
	"context"
	"strconv"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/download"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// subjectsPage asks for one page of a tenant's subjects, newest first.
func subjectsPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "catalog subjects", func(ctx context.Context) (*v1.ListSubjectsResponse, error) {
			return cl.Catalog.ListSubjects(ctx, &v1.ListSubjectsRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListSubjectsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetSubjects()))
			for _, s := range r.GetSubjects() {
				rows = append(rows, row{cells: []string{s.GetLabel(), itoa(s.GetSessionCount()), itoa(s.GetInstanceCount()), output.Human(s.GetSizeBytes()), listTime(s.GetArchiveTime(), loc)}, ref: s})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// sessionsPage asks for one page of a subject's sessions, newest first.
func sessionsPage(c *ctx, owner, gen int, token, tenant, subject string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "catalog sessions", func(ctx context.Context) (*v1.ListSessionsResponse, error) {
			return cl.Catalog.ListSessions(ctx, &v1.ListSessionsRequest{Tenant: tenant, SubjectLabel: subject, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListSessionsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetSessions()))
			for _, s := range r.GetSessions() {
				mods := ""
				for i, m := range s.GetModalities() {
					if i > 0 {
						mods += ","
					}
					mods += m
				}
				rows = append(rows, row{cells: []string{s.GetLabel(), output.Dash(s.GetStudyDate()), output.Dash(mods), itoa(s.GetScanCount()), itoa(s.GetInstanceCount()), output.Human(s.GetSizeBytes()), listTime(s.GetArchiveTime(), loc)}, ref: s})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// scansPage asks for one page of a session's scans, newest first.
func scansPage(c *ctx, owner, gen int, token, tenant, subject, session string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "catalog scans", func(ctx context.Context) (*v1.ListScansResponse, error) {
			return cl.Catalog.ListScans(ctx, &v1.ListScansRequest{Tenant: tenant, SubjectLabel: subject, SessionLabel: session, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListScansResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetScans()))
			for _, s := range r.GetScans() {
				rows = append(rows, row{cells: []string{s.GetLabel(), output.Dash(s.GetModality()), seriesText(s), output.Dash(s.GetSeriesDescription()), itoa(s.GetInstanceCount()), output.Human(s.GetSizeBytes()), listTime(s.GetArchiveTime(), loc)}, ref: s})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// seriesText is a scan's Series Number, or a dash when it has none.
func seriesText(s *v1.Scan) string {
	if s.SeriesNumber == nil {
		return "-"
	}
	return strconv.Itoa(int(s.GetSeriesNumber()))
}

// getScan fetches one scan again.
func getScan(c *ctx, owner int, tenant string, s *v1.Scan) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "scan show", func(ctx context.Context) (*v1.GetScanResponse, error) {
			return cl.Catalog.GetScan(ctx, &v1.GetScanRequest{Tenant: tenant, SubjectLabel: s.GetSubjectLabel(), SessionLabel: s.GetSessionLabel(), ScanLabel: s.GetLabel()})
		})
		return answer(res, err, func(r *v1.GetScanResponse) tea.Msg { return detailMsg{ref: r.GetScan()} })
	})
}

// metadataPage asks for one page of a scan's instance headers, by instance id.
func metadataPage(c *ctx, owner, gen int, token, tenant string, s *v1.Scan) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "scan show", func(ctx context.Context) (*v1.GetScanMetadataResponse, error) {
			return cl.Metadata.GetScanMetadata(ctx, &v1.GetScanMetadataRequest{
				Tenant: tenant, SubjectLabel: s.GetSubjectLabel(), SessionLabel: s.GetSessionLabel(), ScanLabel: s.GetLabel(), PageSize: pageSize, PageToken: token,
			})
		})
		return answer(res, err, func(r *v1.GetScanMetadataResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetInstances()))
			for _, in := range r.GetInstances() {
				rows = append(rows, row{cells: []string{in.GetInstanceId(), output.Dash(in.GetTransferSyntaxUid()), headerState(in)}, ref: in})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// headerState says in a word whether an instance's header is whole.
func headerState(in *v1.InstanceMetadata) string {
	switch {
	case in.GetJsonTooLarge():
		return "too large"
	case in.GetBulkDataOmitted():
		return "no bulk data"
	}
	return "whole"
}

// downloadDoneMsg is the end of a download: what it did, or why it stopped.
type downloadDoneMsg struct {
	res *download.Result
	err error
}

// dlState is how far a download is, shared between the call that runs it and
// the screen that draws it. The call writes it from its own goroutine, so it
// has its own lock.
type dlState struct {
	mu sync.Mutex
	p  download.Progress
}

// set records the progress.
func (s *dlState) set(p download.Progress) {
	s.mu.Lock()
	s.p = p
	s.mu.Unlock()
}

// get reads the progress.
func (s *dlState) get() download.Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p
}

// runDownload starts a download in a call the screen follows rather than
// waits for: it may take hours, and leaving the screen stops it, keeping the
// files that are whole. It has no deadline of its own.
func runDownload(c *ctx, owner int, tenant string, ref []string, dir string, st *dlState, failed func(string)) tea.Cmd {
	cl := c.client
	return c.follow(owner, func(ctx context.Context) tea.Msg {
		res, err := download.Run(ctx, download.Options{Client: cl, Tenant: tenant, Ref: ref, Dir: dir, OnProgress: st.set, OnFailure: failed})
		return downloadDoneMsg{res: res, err: err}
	})
}

// downloadTick is how often a running download is drawn again.
const downloadTick = time.Second
