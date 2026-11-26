package tui

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// This file builds the screens of sending: a tenant's senders with their
// health, its jobs with a progress bar that follows the event feed, a job's
// failures, and the actions that cancel and retry a job.

// yesNoWord is a bool as a word.
func yesNoWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// ---- senders ----

// newSenderList makes the list of a tenant's senders, with their health; it
// refreshes itself on a timer, so a peer that goes down shows without a key
// being pressed.
func newSenderList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Senders of "+clean(tenant), []column{{"NAME", 8, 40}, {"TYPE", 5, 5}, {"TO", 12, 60}, {"ENABLED", 7, 7}, {"HEALTH", 6, 60}},
		"no sender yet; \"nopacs sender add\" makes one")
	l.live = true
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return sendersPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		s, _ := r.ref.(*v1.Sender)
		return push(newSenderDetail(c, tenant, s), nil)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newSenderDetail makes the screen of one sender: where it sends, its limits
// and its health now, with a key that tries its peer. The credential of a stow
// sender is never here: the server answers only whether one is held.
func newSenderDetail(c *ctx, tenant string, s *v1.Sender) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Sender " + clean(s.GetName()), ref: s}
	d.pairs = func(c *ctx, ref any) []pair {
		s, _ := ref.(*v1.Sender)
		ps := []pair{{"Name", s.GetName()}, {"Type", output.SenderType(s)}, {"Enabled", yesNoWord(s.GetEnabled())},
			{"Health", output.SenderHealth(s.GetHealth())}}
		if t := s.GetHealth().GetNextTryTime(); t != nil {
			ps = append(ps, pair{"Next try", output.Time(t, c.loc)})
		}
		if t := s.GetHealth().GetLastSuccessTime(); t != nil {
			ps = append(ps, pair{"Last success", output.Time(t, c.loc)})
		}
		ps = append(ps, pair{"To", output.SenderWhere(s)})
		switch {
		case s.GetDimse() != nil:
			tls := "none (plain TCP)"
			if s.GetDimse().GetTls() != nil {
				tls = "on, CA file " + s.GetDimse().GetTls().GetCaFile()
			}
			ps = append(ps, pair{"Calling AE title", s.GetDimse().GetCallingAeTitle()}, pair{"TLS", tls})
		case s.GetStow() != nil:
			st := s.GetStow()
			ps = append(ps, pair{"Credential held", yesNoWord(st.GetCredentialSet())}, pair{"Max request size", output.Bytes(st.GetMaxRequestBytes())})
		}
		return append(ps, pair{"Max associations", itoa(int64(s.GetMaxAssociations()))}, pair{"Created", output.Time(s.GetCreateTime(), c.loc)})
	}
	d.refetch = func(c *ctx, id int, ref any) tea.Cmd {
		s, _ := ref.(*v1.Sender)
		return getSender(c, id, tenant, s.GetName())
	}
	d.actions = []action{{key: "t", help: "test", do: func(c *ctx, _ screen, ref any) tea.Cmd {
		s, _ := ref.(*v1.Sender)
		ts, cmd := newSenderTest(c, tenant, s)
		return push(ts, cmd)
	}}}
	return d
}

// newSenderTest makes the screen that tries a sender's peer and shows each
// step, then what was found in plain words. A step that fails is a result, not
// an error: the screen says which and why.
func newSenderTest(c *ctx, tenant string, s *v1.Sender) (screen, tea.Cmd) {
	dimse := s.GetDimse() != nil
	d := detailScreen{base: base{c.newID()}, heading: "Test of sender " + clean(s.GetName()), ref: senderTest{res: &v1.TestSenderResponse{}, dimse: dimse}, loading: true}
	d.pairs = func(c *ctx, ref any) []pair {
		t, _ := ref.(senderTest)
		var ps []pair
		for _, r := range t.res.GetResults() {
			v := output.SenderTestOutcome(r.GetOutcome())
			if r.GetDuration() != nil {
				v += ", " + r.GetDuration().AsDuration().Round(time.Millisecond).String()
			}
			if r.GetFault() != v1.SenderFault_SENDER_FAULT_UNSPECIFIED {
				v += ", " + output.SenderFault(r.GetFault())
			}
			if r.GetDetail() != "" {
				v += ": " + r.GetDetail()
			}
			ps = append(ps, pair{output.SenderTestStep(r.GetStep()), v})
		}
		if len(ps) == 0 {
			return []pair{{"Test", "waiting for the peer"}}
		}
		return append(ps, pair{"Found", output.SenderTestSummary(t.res.GetResults(), t.dimse)})
	}
	d.refetch = func(c *ctx, id int, _ any) tea.Cmd { return testSender(c, id, tenant, s.GetName(), dimse) }
	return d, d.refetch(c, d.id, nil)
}

// ---- jobs ----

// jobListScreen is a tenant's jobs, refreshed from its event stream: each job
// event makes the list ask for its first page again, so the progress bars
// follow the archive without a key being pressed. The stream is a call the
// screen does not wait for; leaving the screen stops it.
type jobListScreen struct {
	listScreen
	tenant    string
	following bool
}

// newJobList makes a tenant's jobs list, newest first.
func newJobList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Jobs of "+clean(tenant), []column{{"ID", 8, 26}, {"SENDER", 8, 40}, {"SENDS", 8, 40}, {"STATE", 5, 8},
		{"PROGRESS", 22, 28}, {"SENT", 4, 8}, {"REFUSED", 7, 8}, {"CREATED", 8, 19}},
		"no job yet; \"nopacs job create\" makes one")
	l.live = true
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return jobsPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		j, _ := r.ref.(*v1.Job)
		s, cmd := newJobDetail(c, tenant, j.GetId())
		return push(s, cmd)
	}
	l.note = func(r row) string {
		j, _ := r.ref.(*v1.Job)
		if j.GetReason() != "" {
			return j.GetId() + " is " + output.JobState(j.GetState()) + ": " + j.GetReason()
		}
		return ""
	}
	l.actions = []action{
		{key: "c", help: "cancel", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			j, _ := ref.(*v1.Job)
			return push(newCancelJob(c, tenant, j.GetId()), nil)
		}},
		{key: "r", help: "retry", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			j, _ := ref.(*v1.Job)
			return push(newRetryJob(c, tenant, j.GetId()), nil)
		}},
	}
	nl, cmd := l.start(c)
	return jobListScreen{listScreen: nl, tenant: tenant}, cmd
}

// update applies a message: the first page starts the stream once, an event
// asks for the page again and follows on, a stream that ended follows again
// after a wait; the rest is the list's.
func (s jobListScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case pageMsg:
		next, cmd := s.listScreen.update(c, msg)
		s.listScreen = next.(listScreen)
		if !s.following && s.loaded {
			s.following = true
			return s, tea.Batch(cmd, followJobs(c, s.id, s.tenant, ""))
		}
		return s, cmd
	case jobEventMsg:
		follow := followJobs(c, s.id, s.tenant, msg.cursor)
		if s.loading || c.topID != s.id {
			// Under another screen the list only keeps following; it asks for
			// its page again when its own timer fires on top.
			return s, follow
		}
		nl, fetch := s.reload(c)
		s.listScreen = nl
		return s, tea.Batch(fetch, follow)
	case streamEndedMsg:
		s.following = false
		if c.refresh <= 0 {
			return s, nil
		}
		id := s.id
		return s, tea.Tick(followAgainAfter, func(time.Time) tea.Msg { return followMsg{to: id} })
	case followMsg:
		if s.following {
			return s, nil
		}
		s.following = true
		return s, followJobs(c, s.id, s.tenant, "")
	}
	next, cmd := s.listScreen.update(c, msg)
	if l, ok := next.(listScreen); ok {
		s.listScreen = l
		return s, cmd
	}
	return next, cmd
}

// followMsg tells a screen whose stream ended that its wait is over.
type followMsg struct{ to int }

// jobScreen is one job: its progress as a bar and as counts, refreshed from the
// event stream, with keys that open its failures and cancel or retry it.
type jobScreen struct {
	detailScreen
	tenant string
	cursor string
	// following says a stream is open.
	following bool
}

// newJobDetail makes the screen of one job and asks for it with the position
// of the event feed.
func newJobDetail(c *ctx, tenant, id string) (screen, tea.Cmd) {
	d := detailScreen{base: base{c.newID()}, heading: "Job " + clean(id), ref: &v1.Job{Id: id}, loading: true}
	d.pairs = jobPairs
	d.refetch = func(c *ctx, id int, ref any) tea.Cmd {
		j, _ := ref.(*v1.Job)
		return getJob(c, id, tenant, j.GetId())
	}
	d.actions = []action{
		{key: "f", help: "failures", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			j, _ := ref.(*v1.Job)
			s, cmd := newJobFailureList(c, tenant, j.GetId())
			return push(s, cmd)
		}},
		{key: "c", help: "cancel", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			j, _ := ref.(*v1.Job)
			return push(newCancelJob(c, tenant, j.GetId()), nil)
		}},
		{key: "r", help: "retry", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			j, _ := ref.(*v1.Job)
			return push(newRetryJob(c, tenant, j.GetId()), nil)
		}},
	}
	s := jobScreen{detailScreen: d, tenant: tenant}
	return s, loadJob(c, s.id, tenant, id)
}

// update applies a message: the job read starts the stream once; an event of
// this job moves the bar at once, and one that ends the job reads it again for
// its times and reason; an event of another job is passed over; a stream that
// ended follows again after a wait; the rest is the detail screen's.
func (s jobScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case jobLoadedMsg:
		s.ref, s.loading = msg.job, false
		if !s.following && !output.JobEnded(msg.job.GetState()) {
			s.cursor, s.following = msg.cursor, true
			return s, followJobs(c, s.id, s.tenant, s.cursor)
		}
		return s, nil
	case jobEventMsg:
		s.cursor = msg.cursor
		j, _ := s.ref.(*v1.Job)
		if msg.id != j.GetId() {
			return s, followJobs(c, s.id, s.tenant, s.cursor)
		}
		moved := &v1.Job{Id: j.GetId(), Kind: j.GetKind(), State: j.GetState(), Counts: msg.counts, Reason: j.GetReason(), CreateTime: j.GetCreateTime(),
			StartTime: j.GetStartTime(), StateTime: j.GetStateTime(), EndTime: j.GetEndTime(), CreatedByKeyId: j.GetCreatedByKeyId(), RetryOf: j.GetRetryOf(), RetryJob: j.GetRetryJob()}
		if msg.step {
			moved.State = msg.state
		}
		s.ref = moved
		if output.JobEnded(moved.GetState()) {
			// The job has ended: no more events, and a read for its times.
			s.following, s.loading = false, true
			return s, getJob(c, s.id, s.tenant, j.GetId())
		}
		return s, followJobs(c, s.id, s.tenant, s.cursor)
	case streamEndedMsg:
		s.following = false
		if c.refresh <= 0 {
			return s, nil
		}
		id := s.id
		return s, tea.Tick(followAgainAfter, func(time.Time) tea.Msg { return followMsg{to: id} })
	case followMsg:
		j, _ := s.ref.(*v1.Job)
		if s.following || output.JobEnded(j.GetState()) {
			return s, nil
		}
		s.following = true
		return s, followJobs(c, s.id, s.tenant, s.cursor)
	}
	next, cmd := s.detailScreen.update(c, msg)
	if d, ok := next.(detailScreen); ok {
		s.detailScreen = d
		return s, cmd
	}
	return next, cmd
}

// jobPairs are the lines of a job: its progress bar first, then where it
// sends and how far it got.
func jobPairs(c *ctx, ref any) []pair {
	j, _ := ref.(*v1.Job)
	cn := j.GetCounts()
	ps := []pair{
		{"Id", j.GetId()},
		{"State", output.JobState(j.GetState())},
		{"Progress", output.JobBar(cn, min(max(c.w-20, 12), 44)) + " " + strconv.Itoa(output.JobPercent(cn)) + "%"},
	}
	if s := j.GetSend(); s != nil {
		ps = append(ps, pair{"Sends", output.JobTarget(s)}, pair{"To sender", s.GetSender()})
		if s.GetReception() != "" {
			ps = append(ps, pair{"Forwarded from", "receiver " + s.GetReceiver() + ", reception " + s.GetReception()})
		}
	}
	if ij := j.GetImportJob(); ij != nil {
		ic := ij.GetCounts()
		ps = append(ps, pair{"Imports", ij.GetPath()}, pair{"Receiver", ij.GetReceiver()},
			pair{"Found", itoa(ic.GetFound())}, pair{"Handed in", itoa(ic.GetHandedIn())},
			pair{"Archived", itoa(ic.GetArchived())}, pair{"Already held", itoa(ic.GetAlreadyHeld())},
			pair{"Processing", itoa(ic.GetProcessing())})
	}
	for _, p := range output.ReplicationJobPairs(j) {
		ps = append(ps, pair{p.Label, p.Value})
	}
	if sj := j.GetScrub(); sj != nil {
		ps = append(ps, pair{"Scrubs", output.Dash(sj.GetStorage())})
	}
	if rj := j.GetRebuild(); rj != nil {
		ps = append(ps, pair{"Rebuilds", "the catalog, " + strings.ToLower(strings.TrimPrefix(rj.GetMode().String(), "REBUILD_JOB_MODE_"))}, pair{"From storage", output.Dash(rj.GetFromStorage())})
	}
	if j.GetReason() != "" {
		ps = append(ps, pair{"Reason", j.GetReason()})
	}
	ps = append(ps,
		pair{"Instances", itoa(cn.GetInstances()) + ", " + output.Human(cn.GetBytes())},
		pair{"Sent", itoa(cn.GetInstancesSent()) + ", " + output.Human(cn.GetBytesSent())},
		pair{"Warned", itoa(cn.GetInstancesWarned())},
		pair{"Refused", itoa(cn.GetInstancesRefused()) + ", " + output.Human(cn.GetBytesRefused())},
		pair{"Remaining", itoa(cn.GetInstancesRemaining()) + ", " + output.Human(cn.GetBytesRemaining())},
		pair{"Scans", itoa(cn.GetScansDone()) + " of " + itoa(cn.GetScans()) + " done"},
		pair{"Created", output.Time(j.GetCreateTime(), c.loc)},
		pair{"Ended", output.Time(j.GetEndTime(), c.loc)})
	if !cn.GetListed() {
		ps = append(ps, pair{"Note", "still listing the scans: the total grows"})
	}
	if j.GetRetryJob() != "" {
		ps = append(ps, pair{"Retried as", j.GetRetryJob()})
	}
	if j.GetRetryOf() != "" {
		ps = append(ps, pair{"Retry of", j.GetRetryOf()})
	}
	return ps
}

// newJobFailureList makes the list of what a job did not send, with the
// instances the peer kept with a warning.
func newJobFailureList(c *ctx, tenant, id string) (screen, tea.Cmd) {
	l := newList(c, "Failures of job "+clean(id), []column{{"SCAN", 8, 40}, {"INSTANCE", 8, 26}, {"WHY", 8, 20}, {"STATUS", 6, 6}, {"DETAIL", 8, 60}, {"TIME", 8, 19}},
		"the job has no failure and no warning")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return jobFailuresPage(c, l.id, l.gen, token, tenant, id)
	}
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		f, _ := r.ref.(*v1.JobFailure)
		d := detailScreen{base: base{c.newID()}, heading: "Failure of " + clean(f.GetInstanceId()), ref: f, pairs: func(c *ctx, ref any) []pair {
			f, _ := ref.(*v1.JobFailure)
			status := "-"
			if f.GetPeerStatus() != 0 {
				status = strings.ToUpper(strconv.FormatInt(int64(f.GetPeerStatus()), 16))
			}
			return []pair{{"Instance", f.GetInstanceId()}, {"Scan", f.GetSubjectLabel() + "/" + f.GetSessionLabel() + "/" + f.GetScanLabel()},
				{"Why", output.JobFailureCode(f.GetCode())}, {"Peer status", status}, {"Detail", output.Dash(f.GetDetail())},
				{"SOP class", output.Dash(f.GetSopClassUid())}, {"Transfer syntax", output.Dash(f.GetTransferSyntaxUid())}, {"Time", output.Time(f.GetTime(), c.loc)}}
		}}
		return push(d, nil)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newCancelJob asks for the job's id to be typed, then cancels it: units not
// started end at once and one that is sending ends within a batch.
func newCancelJob(c *ctx, tenant, id string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Cancel job " + clean(id),
		intro: []string{
			"This ends the job as canceled: what the peer has answered stays counted, and what was not sent can be sent again with retry.",
		},
		fields:   []formField{{label: "Type the id", hint: id}},
		want:     id,
		confirms: true,
		verb:     "cancel",
		pop:      1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) { return cancelJob(c, f.id, tenant, id), nil }
	return f
}

// newRetryJob asks for the job's id to be typed, then makes the job that sends
// what the ended one did not send, to the same sender.
func newRetryJob(c *ctx, tenant, id string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Retry job " + clean(id),
		intro: []string{
			"This makes a new job that sends the instances this ended job did not send, to the same sender.",
			"The ended job stays as it is and names the new one.",
		},
		fields:   []formField{{label: "Type the id", hint: id}},
		want:     id,
		confirms: true,
		verb:     "retry",
		pop:      1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if id == "" {
			return nil, fault.New(fault.Invalid, "no job")
		}
		return retryJob(c, f.id, tenant, id), nil
	}
	return f
}
