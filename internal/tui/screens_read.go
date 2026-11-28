package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/download"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
)

// downloadAction is the key that downloads what a list row or a scan stands
// for: ref says the subject, then the session and scan when they apply.
func downloadAction(tenant string, ref func(any) []string) action {
	return action{key: "d", help: "download", needsRow: true, do: func(c *ctx, _ screen, r any) tea.Cmd {
		return push(newDownloadForm(c, tenant, ref(r)), nil)
	}}
}

// newSubjectList makes the list of a tenant's subjects, newest first; a
// subject opens on its sessions.
func newSubjectList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Subjects of "+clean(tenant), []column{{"SUBJECT", 8, 40}, {"SESSIONS", 8, 8}, {"INSTANCES", 9, 10}, {"SIZE", 8, 10}, {"FIRST ARCHIVED", 14, 19}},
		"no subject yet: nothing has been archived")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return subjectsPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		s, _ := r.ref.(*v1.Subject)
		ns, cmd := newSessionList(c, tenant, s.GetLabel())
		return push(ns, cmd)
	}
	l.actions = []action{downloadAction(tenant, func(r any) []string { s, _ := r.(*v1.Subject); return []string{s.GetLabel()} })}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newSessionList makes the list of a subject's sessions, newest first; a
// session opens on its scans.
func newSessionList(c *ctx, tenant, subject string) (screen, tea.Cmd) {
	l := newList(c, "Sessions of "+clean(subject), []column{{"SESSION", 7, 40}, {"STUDY DATE", 10, 10}, {"MODALITIES", 10, 20}, {"SCANS", 5, 6}, {"INSTANCES", 9, 10}, {"SIZE", 8, 10}, {"ARCHIVED", 8, 19}},
		"this subject has no session")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return sessionsPage(c, l.id, l.gen, token, tenant, subject)
	}
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		s, _ := r.ref.(*v1.Session)
		ns, cmd := newScanList(c, tenant, subject, s.GetLabel())
		return push(ns, cmd)
	}
	l.actions = []action{downloadAction(tenant, func(r any) []string { s, _ := r.(*v1.Session); return []string{subject, s.GetLabel()} })}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newScanList makes the list of a session's scans, newest first; a scan opens
// on its detail.
func newScanList(c *ctx, tenant, subject, session string) (screen, tea.Cmd) {
	l := newList(c, "Scans of "+clean(subject)+"/"+clean(session), []column{{"SCAN", 6, 40}, {"MODALITY", 8, 8}, {"SERIES", 6, 6}, {"DESCRIPTION", 11, 40}, {"INSTANCES", 9, 10}, {"SIZE", 8, 10}, {"ARCHIVED", 8, 19}},
		"this session has no scan")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return scansPage(c, l.id, l.gen, token, tenant, subject, session)
	}
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		s, _ := r.ref.(*v1.Scan)
		return push(newScanDetail(c, tenant, s), nil)
	}
	l.actions = []action{downloadAction(tenant, func(r any) []string { s, _ := r.(*v1.Scan); return []string{subject, session, s.GetLabel()} })}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newScanDetail makes the screen of one scan: its catalog row, with the keys
// that show its headers and download it.
func newScanDetail(c *ctx, tenant string, s *v1.Scan) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Scan " + clean(s.GetSubjectLabel()) + "/" + clean(s.GetSessionLabel()) + "/" + clean(s.GetLabel()), ref: s}
	d.pairs = func(c *ctx, ref any) []pair {
		s, _ := ref.(*v1.Scan)
		return []pair{
			{"Series UID", output.Value(s.GetValuesWithheld(), s.GetSeriesInstanceUid())}, {"Modality", output.Value(s.GetValuesWithheld(), s.GetModality())}, {"Series number", seriesText(s)},
			{"Description", output.Value(s.GetValuesWithheld(), s.GetSeriesDescription())}, {"Instances", itoa(s.GetInstanceCount())}, {"Size", output.Human(s.GetSizeBytes())},
			{"Storage", output.Dash(s.GetStorage())}, {"Manifest SHA-256", output.Dash(s.GetManifestSha256())}, {"Receiver", output.Dash(s.GetReceiver())},
			{"Archived", output.Time(s.GetArchiveTime(), c.loc)}, {"Updated", output.Time(s.GetUpdateTime(), c.loc)},
		}
	}
	d.refetch = func(c *ctx, id int, ref any) tea.Cmd {
		s, _ := ref.(*v1.Scan)
		return getScan(c, id, tenant, s)
	}
	d.actions = []action{
		{key: "m", help: "headers", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			s, _ := ref.(*v1.Scan)
			ns, cmd := newHeaderList(c, tenant, s)
			return push(ns, cmd)
		}},
		{key: "d", help: "download", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			s, _ := ref.(*v1.Scan)
			return push(newDownloadForm(c, tenant, []string{s.GetSubjectLabel(), s.GetSessionLabel(), s.GetLabel()}), nil)
		}},
	}
	return d
}

// newHeaderList makes the list of a scan's instances, a page at a time; an
// instance opens on its DICOM header.
func newHeaderList(c *ctx, tenant string, s *v1.Scan) (screen, tea.Cmd) {
	l := newList(c, "Headers of "+clean(s.GetLabel()), []column{{"INSTANCE", 8, 64}, {"TRANSFER SYNTAX", 15, 32}, {"HEADER", 6, 12}},
		"this scan has no instance")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return metadataPage(c, l.id, l.gen, token, tenant, s)
	}
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		in, _ := r.ref.(*v1.InstanceMetadata)
		return push(newInstanceHeader(c, in), nil)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newInstanceHeader makes the screen of one instance's header: one line per
// attribute, in tag order, as the DICOM JSON model carries them.
func newInstanceHeader(c *ctx, in *v1.InstanceMetadata) screen {
	return detailScreen{base: base{c.newID()}, heading: "Header of " + clean(in.GetInstanceId()), ref: in, pairs: func(_ *ctx, ref any) []pair {
		in, _ := ref.(*v1.InstanceMetadata)
		return headerPairs(in)
	}}
}

// maxHeaderValue is the longest value drawn for one attribute; a longer one
// is cut, since a screen shows what a person reads, and the command prints
// the whole header.
const maxHeaderValue = 300

// headerPairs turns an instance's DICOM JSON into labeled lines.
func headerPairs(in *v1.InstanceMetadata) []pair {
	ps := []pair{{"Instance", in.GetInstanceId()}, {"SOP instance UID", output.Dash(in.GetSopInstanceUid())}, {"Transfer syntax", output.Dash(in.GetTransferSyntaxUid())}}
	switch {
	case in.GetJsonTooLarge():
		return append(ps, pair{"Header", "not shown: it is longer than 1 MiB; \"nopacs download\" gets the file"})
	case in.GetDicomJson() == "":
		return append(ps, pair{"Header", "none"})
	}
	var tags map[string]json.RawMessage
	if err := json.Unmarshal([]byte(in.GetDicomJson()), &tags); err != nil {
		return append(ps, pair{"Header", "could not be read as DICOM JSON"})
	}
	if in.GetBulkDataOmitted() {
		ps = append(ps, pair{"Note", "bulk data is left out: the file is in the deflated transfer syntax"})
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ps = append(ps, pair{tagLabel(k), attributeText(tags[k])})
	}
	return ps
}

// tagLabel writes a tag key of the DICOM JSON model, eight hexadecimal digits,
// as (gggg,eeee).
func tagLabel(k string) string {
	if len(k) == 8 {
		return "(" + k[:4] + "," + k[4:] + ")"
	}
	return k
}

// attributeText writes one attribute: its value representation and its
// values. A sequence is only counted, and bulk data only named.
func attributeText(raw json.RawMessage) string {
	var a struct {
		VR          string            `json:"vr"`
		Value       []json.RawMessage `json:"Value"`
		BulkDataURI string            `json:"BulkDataURI"`
		Inline      string            `json:"InlineBinary"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "?"
	}
	switch {
	case a.BulkDataURI != "":
		return a.VR + ": bulk data"
	case a.Inline != "":
		return a.VR + ": binary"
	case a.VR == "SQ":
		return fmt.Sprintf("SQ: %d item(s)", len(a.Value))
	}
	vals := make([]string, 0, len(a.Value))
	for _, v := range a.Value {
		var s string
		var p struct {
			Alphabetic string `json:"Alphabetic"`
		}
		switch {
		case json.Unmarshal(v, &s) == nil:
			vals = append(vals, s)
		case json.Unmarshal(v, &p) == nil && p.Alphabetic != "":
			vals = append(vals, p.Alphabetic)
		default:
			vals = append(vals, string(v))
		}
	}
	out := a.VR + ": " + strings.Join(vals, `\`)
	if len(out) > maxHeaderValue {
		out = out[:maxHeaderValue] + ellipsis
	}
	return out
}

// newDownloadForm asks for the directory to download into and starts the
// download on the screen that replaces it.
func newDownloadForm(c *ctx, tenant string, ref []string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Download " + clean(strings.Join(ref, "/")),
		intro: []string{
			"The files are written to <directory>/<subject>/<session>/<scan>/<instance>.dcm, each checked against its SHA-256 as it arrives.",
			"They hold what the sender sent, identifying data included. Running it again continues where it stopped.",
		},
		fields: []formField{{label: "Directory", hint: "where the files go; made when missing", in: field{}.set("./nopacs-download")}},
		verb:   "download",
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		dir := f.value(0)
		if dir == "" {
			return nil, fault.New(fault.Invalid, "say which directory to download into")
		}
		for _, p := range ref {
			if err := label.Check(p); err != nil {
				return nil, fault.New(fault.Invalid, "not a label: "+p)
			}
		}
		s, cmd := newDownload(c, tenant, ref, dir)
		return func() tea.Msg { return replaceMsg{s: s, cmd: cmd} }, nil
	}
	return f
}

// downloadScreen shows a download as it runs: how many files are whole, how
// many were already, how many bytes came, and the file being written; and how
// it ended. The download is a call the screen follows, so leaving the screen
// stops it and keeps the files that are whole.
type downloadScreen struct {
	base
	tenant string
	ref    []string
	dir    string
	st     *dlState
	// failed collects the files removed for not matching, as they happen.
	failed *failedFiles
	done   bool
	res    *download.Result
	err    error
}

// failedFiles is a list written by the download's goroutine and read by the
// screen.
type failedFiles struct {
	mu   sync.Mutex
	list []string
}

// add records a failure.
func (f *failedFiles) add(s string) {
	f.mu.Lock()
	f.list = append(f.list, s)
	f.mu.Unlock()
}

// all returns the failures so far.
func (f *failedFiles) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.list...)
}

// newDownload makes the screen and starts the download.
func newDownload(c *ctx, tenant string, ref []string, dir string) (screen, tea.Cmd) {
	s := downloadScreen{base: base{c.newID()}, tenant: tenant, ref: ref, dir: dir, st: &dlState{}, failed: &failedFiles{}}
	cmd := runDownload(c, s.id, tenant, ref, dir, s.st, s.failed.add)
	if c.refresh > 0 {
		cmd = tea.Batch(cmd, tea.Tick(downloadTick, func(time.Time) tea.Msg { return tickMsg{to: s.id} }))
	}
	return s, cmd
}

// update applies a message.
func (s downloadScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case downloadDoneMsg:
		s.done, s.res, s.err = true, msg.res, msg.err
	case tickMsg:
		if s.done {
			return s, nil
		}
		id := s.id
		return s, tea.Tick(downloadTick, func(time.Time) tea.Msg { return tickMsg{to: id} })
	case tea.KeyPressMsg:
		if msg.String() == "esc" {
			return s, back(false, "")
		}
	}
	return s, nil
}

// title names the screen.
func (s downloadScreen) title(*ctx) string { return "Download " + clean(strings.Join(s.ref, "/")) }

// overallProgress is the one line of the whole download's progress: the files
// settled of those the set holds, the bytes, the rate over the last ten
// seconds and the time left at that rate, the same numbers the command's line
// shows.
func overallProgress(p download.Progress) string {
	if p.Phase == download.PhaseListing {
		return fmt.Sprintf("listing the files: %d found", p.FilesTotal)
	}
	text := fmt.Sprintf("%d of %d files, %s of %s", p.Finished(), p.FilesTotal, output.Human(p.Bytes), output.Human(p.BytesTotal))
	if p.Rate > 0 {
		text += ", " + output.Human(int64(p.Rate)) + "/s"
		if p.Remaining >= 0 {
			text += ", " + p.Remaining.Round(time.Second).String() + " left"
		}
	}
	return text
}

// editing is false.
func (s downloadScreen) editing() bool { return false }

// keys lists the keys.
func (s downloadScreen) keys() []keyHelp {
	if s.done {
		return []keyHelp{{"esc", "back"}}
	}
	return []keyHelp{{"esc", "stop and go back"}}
}

// view draws the progress or the result.
func (s downloadScreen) view(c *ctx) []string {
	p := s.st.get()
	lines := []string{
		c.st.bold("Tenant:") + " " + clean(s.tenant),
		c.st.bold("Into:") + "   " + clean(s.dir),
	}
	files, skipped, bytes := p.Files, p.Skipped, p.Bytes
	if s.res != nil {
		files, skipped, bytes = s.res.Files, s.res.Skipped, s.res.Bytes
	}
	lines = append(lines, "",
		fmt.Sprintf("%d files fetched, %d already complete, %s received", files, skipped, output.Human(bytes)))
	if !s.done && p.FilesTotal > 0 {
		lines = append(lines, overallProgress(p))
	}
	switch {
	case !s.done:
		if p.Current != "" {
			lines = append(lines, c.st.faint("writing "+trunc(clean(p.Current), max(c.w-8, 10))))
		}
		lines = append(lines, "", c.st.faint("esc stops it; the files that are whole stay, and downloading again continues"))
	case s.err != nil:
		lines = append(lines, "", c.st.bad("stopped: "+clean(fault.Message(s.err))))
	default:
		lines = append(lines, "", c.st.good("done"))
	}
	for _, f := range s.failed.all() {
		lines = append(lines, c.st.bad("failed its check and was removed: "+clean(f)))
	}
	var out []string
	for _, l := range lines {
		out = append(out, wrap(l, c.w)...)
	}
	return out
}
