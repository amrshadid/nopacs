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

// itoa writes n in decimal.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// tenantActions are the keys of a tenant's screen that open what it holds:
// its status, storages, receivers, senders, jobs, receptions and quarantined
// files. The key for jobs is not j: on a detail screen j scrolls.
func tenantActions(c *ctx, name string) []action {
	open := func(make func(c *ctx, tenant string) (screen, tea.Cmd)) func(c *ctx, _ screen, _ any) tea.Cmd {
		return func(c *ctx, _ screen, _ any) tea.Cmd {
			s, cmd := make(c, name)
			return push(s, cmd)
		}
	}
	return []action{
		{key: "t", help: "status", need: v1.Role_ROLE_READER, do: open(newStatus)},
		{key: "o", help: "storages", need: v1.Role_ROLE_READER, do: open(newStorageList)},
		{key: "r", help: "receivers", need: v1.Role_ROLE_READER, do: open(newReceiverList)},
		{key: "n", help: "senders", need: v1.Role_ROLE_READER, do: open(newSenderList)},
		{key: "w", help: "jobs", need: v1.Role_ROLE_READER, do: open(newJobList)},
		{key: "e", help: "receptions", need: v1.Role_ROLE_READER, do: open(newReceptionList)},
		{key: "h", help: "quarantine", need: v1.Role_ROLE_READER, do: open(newQuarantineList)},
		{key: "c", help: "catalog", need: v1.Role_ROLE_READER, do: open(newSubjectList)},
		{key: "b", help: "replicas", need: v1.Role_ROLE_READER, do: open(newReplicaList)},
		{key: "i", help: "retention", need: v1.Role_ROLE_READER, do: open(newRuleList)},
		{key: "l", help: "holds", need: v1.Role_ROLE_OPERATOR, do: open(newHoldList)},
		{key: "x", help: "corrections", need: v1.Role_ROLE_OPERATOR, do: open(newCorrectionList)},
		{key: "f", help: "findings", need: v1.Role_ROLE_OPERATOR, do: open(newFindingList)},
		{key: "p", help: "profiles", need: v1.Role_ROLE_OPERATOR, do: open(newProfileList)},
		{key: "v", help: "audit seal", need: v1.Role_ROLE_ADMIN, do: func(c *ctx, _ screen, _ any) tea.Cmd {
			s, cmd := newSealVerify(c, tenantScope(name))
			return push(s, cmd)
		}},
	}
}

// allowed keeps the keys of a tenant's screen that the caller's grants allow:
// a key that asks a role on the tenant, or on the installation, the caller does
// not hold is left out, so a reader is not offered what only an admin may do.
func allowed(c *ctx, tenant string, actions []action) []action {
	var out []action
	for _, a := range actions {
		switch {
		case a.installation && !c.acc.onInstallation(v1.Role_ROLE_ADMIN):
		case a.need != 0 && !c.acc.onTenant(tenant, a.need):
		default:
			out = append(out, a)
		}
	}
	return out
}

// ---- status ----

// statusScreen is a tenant's status, refreshed from its event stream: each
// event the tenant commits makes the screen ask for the status again, so
// what it shows follows the archive without a key being pressed. The stream
// is a call the screen does not wait for; leaving the screen stops it.
type statusScreen struct {
	detailScreen
	tenant string
	// cursor is where the stream follows from: the status's event cursor,
	// then the last event's.
	cursor string
	// following says a stream is open.
	following bool
}

// newStatus makes a tenant's status screen and asks for the status.
func newStatus(c *ctx, tenant string) (screen, tea.Cmd) {
	d := detailScreen{base: base{c.newID()}, heading: "Status of " + clean(tenant), ref: &v1.TenantStatus{Tenant: tenant}, loading: true}
	d.pairs = statusPairs
	d.refetch = func(c *ctx, id int, _ any) tea.Cmd { return getStatus(c, id, tenant) }
	s := statusScreen{detailScreen: d, tenant: tenant}
	return s, getStatus(c, s.id, tenant)
}

// update applies a message: a status starts the stream once, an event
// asks for the status again and follows on, a stream that ended follows
// again after a wait; the rest is the detail screen's.
func (s statusScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case detailMsg:
		st, _ := msg.ref.(*v1.TenantStatus)
		s.ref, s.loading = st, false
		if !s.following && st.GetEventCursor() != "" {
			s.cursor, s.following = st.GetEventCursor(), true
			return s, followEvents(c, s.id, s.tenant, s.cursor)
		}
		return s, nil
	case eventSeenMsg:
		s.cursor = msg.cursor
		follow := followEvents(c, s.id, s.tenant, s.cursor)
		if s.loading {
			return s, follow
		}
		s.loading = true
		return s, tea.Batch(getStatus(c, s.id, s.tenant), follow)
	case streamEndedMsg:
		s.following = false
		if c.refresh <= 0 {
			// Screens do not refresh themselves (Options.NoRefresh).
			return s, nil
		}
		id := s.id
		return s, tea.Tick(followAgainAfter, func(time.Time) tea.Msg { return tickMsg{to: id} })
	case tickMsg:
		if s.following || s.cursor == "" {
			return s, nil
		}
		s.following = true
		return s, followEvents(c, s.id, s.tenant, s.cursor)
	}
	next, cmd := s.detailScreen.update(c, msg)
	if d, ok := next.(detailScreen); ok {
		s.detailScreen = d
		return s, cmd
	}
	return next, cmd
}

// statusPairs are the lines of a status.
func statusPairs(c *ctx, ref any) []pair {
	st, _ := ref.(*v1.TenantStatus)
	ps := []pair{{"Database", output.State(st.GetDatabaseState())}}
	if n := st.GetCounts(); n != nil {
		ps = append(ps,
			pair{"Catalog", itoa(n.GetSubjects()) + " subjects, " + itoa(n.GetSessions()) + " sessions, " + itoa(n.GetScans()) + " scans, " + itoa(n.GetInstances()) + " instances"},
			pair{"Quarantine", itoa(n.GetQuarantinedFiles()) + " files held, " + output.Bytes(n.GetQuarantinedBytes())},
			pair{"Incoming", output.Bytes(n.GetIncomingBytes())})
		var steps []string
		for _, sc := range n.GetReceptionsInProgress() {
			steps = append(steps, itoa(sc.GetReceptions())+" "+output.ReceptionStep(sc.GetStep()))
		}
		ps = append(ps, pair{"In progress", output.Dash(strings.Join(steps, ", "))})
	} else {
		ps = append(ps, pair{"Counts", "not read: the tenant's database does not answer"})
	}
	if limit := st.GetIncomingLimitBytes(); limit > 0 {
		ps = append(ps, pair{"Incoming limit", output.Bytes(limit)})
	}
	if tp := st.GetThroughput(); tp != nil {
		ps = append(ps, pair{"Received", strconv.FormatFloat(tp.GetFilesReceivedPerSecond(), 'f', 1, 64) + " files/s, " + output.Bytes(int64(tp.GetBytesReceivedPerSecond())) + "/s"},
			pair{"Archived", strconv.FormatFloat(tp.GetFilesArchivedPerSecond(), 'f', 1, 64) + " files/s, " + output.Bytes(int64(tp.GetBytesArchivedPerSecond())) + "/s"})
	}
	for _, s := range st.GetStorages() {
		v := output.Bytes(s.GetUsedBytes()) + " used"
		if s.FreeBytes != nil {
			v += ", " + output.Bytes(s.GetFreeBytes()) + " free"
		}
		if s.GetNearlyFull() {
			v += ", nearly full"
		}
		v += ", " + output.StorageTier(s.GetTier()) + " tier"
		if s.GetReplicaOf() != "" {
			v += ", replica of " + s.GetReplicaOf()
		}
		if s.GetSwitching() {
			v += ", SWITCHING: its receivers refuse new data"
		}
		v += ", encrypted at rest: " + output.Encryption(s.GetEncryptionAtRest(), s.GetEncryptionAlgorithm())
		if cv := s.GetCoverage(); cv != nil {
			v += ", scrubbed: " + output.Coverage(cv)
		}
		ps = append(ps, pair{"Storage " + s.GetStorage(), v})
	}
	for _, r := range st.GetReplicas() {
		ps = append(ps, pair{"Replica " + r.GetReplica(), "of " + r.GetPrimary() + ", " + itoa(r.GetScansInStep()) + " scans in step, " + itoa(r.GetScansPending()) + " waiting"})
	}
	if rs := st.GetRetention(); rs != nil {
		v := itoa(rs.GetRules()) + " rules, " + itoa(rs.GetSessionsDue()) + " sessions due, " + itoa(rs.GetActiveHolds()) + " holds"
		if rs.GetPaused() {
			v += ", PAUSED: " + rs.GetPauseReason()
		}
		ps = append(ps, pair{"Retention", v})
	}
	for _, r := range st.GetReceivers() {
		v := output.ReceiverHealth(r.GetHealth())
		if r.GetAnyCaller() {
			v += ", admits any caller"
		}
		ps = append(ps, pair{"Receiver " + r.GetReceiver(), v})
	}
	for _, s := range st.GetSenders() {
		ps = append(ps, pair{"Sender " + s.GetSender(), output.SenderHealth(s.GetHealth())})
	}
	return ps
}

// ---- storages, receivers, receptions ----

// newStorageList makes the list of a tenant's storages.
func newStorageList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Storages of "+clean(tenant), []column{{"NAME", 8, 40}, {"TYPE", 9, 9}, {"LOCATION", 12, 60}, {"CAPACITY", 8, 12}},
		"no storage yet; \"nopacs storage add\" makes one")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return storagesPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		st, _ := r.ref.(*v1.Storage)
		d := detailScreen{base: base{c.newID()}, heading: "Storage " + clean(st.GetName()), ref: st, pairs: func(c *ctx, ref any) []pair {
			st, _ := ref.(*v1.Storage)
			return []pair{{"Name", st.GetName()}, {"Type", output.StorageType(st)}, {"Location", output.StorageWhere(st)},
				{"Tier", output.StorageTier(st.GetTier())}, {"Replica of", output.Dash(st.GetReplicaOf())},
				{"Capacity", output.Bytes(st.GetCapacityBytes())}, {"Created", output.Time(st.GetCreateTime(), c.loc)}}
		}}
		return push(d, nil)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newReceiverList makes the list of a tenant's receivers, with their
// health.
func newReceiverList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Receivers of "+clean(tenant), []column{{"NAME", 8, 40}, {"PORT", 5, 5}, {"AE TITLE", 8, 16}, {"STORAGE", 8, 40}, {"HEALTH", 6, 60}},
		"no receiver yet; \"nopacs receiver add\" makes one")
	l.live = true
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return receiversPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		rc, _ := r.ref.(*v1.Receiver)
		d := detailScreen{base: base{c.newID()}, heading: "Receiver " + clean(rc.GetName()), ref: rc, pairs: func(c *ctx, ref any) []pair {
			rc, _ := ref.(*v1.Receiver)
			if im := rc.GetImporter(); im != nil {
				// An import receiver holds no listener: its root and layout.
				return []pair{{"Name", rc.GetName()}, {"Type", "import"}, {"Enabled", strconv.FormatBool(rc.GetEnabled())},
					{"Health", output.ReceiverHealth(rc.GetHealth())}, {"Root", im.GetRoot()},
					{"Layout", strings.ToLower(strings.TrimPrefix(im.GetLayout().String(), "IMPORT_LAYOUT_"))}, {"Storage", rc.GetStorage()}}
			}
			if st := rc.GetStow(); st != nil {
				keys := "no key: none is listed"
				if n := len(st.GetKeyIds()); n > 0 {
					keys = itoa(int64(n)) + " key(s)"
				}
				return []pair{{"Name", rc.GetName()}, {"Type", "stow"}, {"Enabled", strconv.FormatBool(rc.GetEnabled())},
					{"Health", output.ReceiverHealth(rc.GetHealth())}, {"Takes", keys}, {"Storage", rc.GetStorage()}}
			}
			dm := rc.GetDimse()
			callers := "nobody: no caller is listed"
			if dm.GetAnyCaller() {
				callers = "any caller"
			} else if n := len(dm.GetAllowedCallers().GetRules()); n > 0 {
				callers = itoa(int64(n)) + " rule(s)"
			}
			return []pair{{"Name", rc.GetName()}, {"Enabled", strconv.FormatBool(rc.GetEnabled())}, {"Health", output.ReceiverHealth(rc.GetHealth())},
				{"Port", itoa(int64(dm.GetPort()))}, {"AE title", dm.GetAeTitle()}, {"Admits", callers}, {"Storage", rc.GetStorage()}}
		}}
		d.actions = []action{{key: "u", help: "routes", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			rc, _ := ref.(*v1.Receiver)
			s, cmd := newRouteList(c, tenant, rc.GetName())
			return push(s, cmd)
		}}}
		return push(d, nil)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newReceptionList makes the list of a tenant's receptions, newest first.
func newReceptionList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Receptions of "+clean(tenant), []column{{"ID", 8, 26}, {"RECEIVER", 8, 40}, {"STEP", 8, 12},
		{"RECEIVED", 8, 8}, {"ARCHIVED", 8, 8}, {"QUARANTINED", 11, 11}, {"STARTED", 8, 19}},
		"no reception yet")
	l.live = true
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return receptionsPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		rc, _ := r.ref.(*v1.Reception)
		d := detailScreen{base: base{c.newID()}, heading: "Reception " + clean(rc.GetId()), ref: rc, pairs: func(c *ctx, ref any) []pair {
			rc, _ := ref.(*v1.Reception)
			n := rc.GetCounts()
			ps := []pair{{"Id", rc.GetId()}, {"Receiver", rc.GetReceiver()}, {"Step", output.ReceptionStep(rc.GetStep())},
				{"Files", itoa(n.GetFilesReceived()) + " received, " + itoa(n.GetFilesArchived()) + " archived, " +
					itoa(n.GetFilesDuplicate()) + " duplicate, " + itoa(n.GetFilesQuarantined()) + " quarantined"},
				{"Subject", output.Dash(rc.GetSubjectLabel())}, {"Session", output.Dash(rc.GetSessionLabel())},
				{"Started", output.Time(rc.GetStartTime(), c.loc)}}
			if rc.GetFailure() != "" {
				ps = append(ps, pair{"Failure", rc.GetFailure()})
			}
			return ps
		}}
		return push(d, nil)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// ---- quarantine ----

// newQuarantineList makes the list of a tenant's quarantined files held
// now, newest first; a file opens on its detail, where it is released or
// rejected.
func newQuarantineList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Quarantine of "+clean(tenant), []column{{"ID", 8, 26}, {"REASON", 8, 40}, {"RECEIVER", 8, 40}, {"BYTES", 5, 10}, {"QUARANTINED", 11, 19}},
		"no file is held in quarantine")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return quarantinePage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		f, _ := r.ref.(*v1.QuarantinedFile)
		return push(newQuarantineDetail(c, tenant, f), nil)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newQuarantineDetail makes the screen of one quarantined file, with the
// keys that release it or reject it, each of which asks first.
func newQuarantineDetail(c *ctx, tenant string, f *v1.QuarantinedFile) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Quarantined file " + clean(f.GetId()), ref: f}
	d.pairs = func(c *ctx, ref any) []pair {
		f, _ := ref.(*v1.QuarantinedFile)
		return []pair{{"Id", f.GetId()}, {"Reason", output.QuarantineReason(f.GetReason())}, {"State", output.QuarantineState(f.GetState())},
			{"Receiver", f.GetReceiver()}, {"Reception", output.Dash(f.GetReception())}, {"Size", output.Bytes(f.GetSizeBytes())},
			{"Bytes kept", strconv.FormatBool(f.GetContentKept())}, {"Quarantined", output.Time(f.GetQuarantineTime(), c.loc)}}
	}
	d.actions = []action{
		{key: "r", help: "release", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			f, _ := ref.(*v1.QuarantinedFile)
			return push(newReleaseFile(c, tenant, f.GetId()), nil)
		}},
		{key: "d", help: "reject", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			f, _ := ref.(*v1.QuarantinedFile)
			return push(newRejectFile(c, tenant, f.GetId()), nil)
		}},
	}
	return d
}

// newReleaseFile asks why a file is released, and releases it as it is: it
// is received again into its receiver and checked as it first was.
func newReleaseFile(c *ctx, tenant, id string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Release " + clean(id),
		intro: []string{
			"The file is received again by its receiver and checked as it was the first time.",
			"The reason is kept with its record.",
		},
		fields: []formField{{label: "Reason", hint: "why it is released"}},
		verb:   "release",
		pop:    2,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		reason := f.value(0)
		if reason == "" {
			return nil, fault.New(fault.Invalid, "say why the file is released")
		}
		return releaseFile(c, f.id, tenant, id, reason), nil
	}
	return f
}

// newRejectFile asks for the reason and for the file's id typed again, then
// rejects it: its bytes are removed for good and its record stays.
func newRejectFile(c *ctx, tenant, id string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Reject " + clean(id),
		intro: []string{
			"This removes the file's bytes for good; its record stays, with the reason.",
		},
		fields:   []formField{{label: "Type the id", hint: id}, {label: "Reason", hint: "why it is rejected"}},
		want:     id,
		confirms: true,
		verb:     "reject",
		pop:      2,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		reason := f.value(1)
		if reason == "" {
			return nil, fault.New(fault.Invalid, "say why the file is rejected")
		}
		return rejectFile(c, f.id, tenant, id, reason), nil
	}
	return f
}
