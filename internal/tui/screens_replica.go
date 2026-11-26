package tui

import (
	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newReplicaList makes the list of a tenant's replicas and how far each is
// from its primary. Its keys attach a storage, detach and backfill a replica,
// repair the replica's primary from the findings on it, and switch the tenant
// onto the replica; each of those asks first.
func newReplicaList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Replicas of "+clean(tenant), []column{{"REPLICA", 8, 40}, {"PRIMARY", 8, 40}, {"TIER", 6, 10}, {"IN STEP", 7, 9},
		{"WAITING", 7, 9}, {"DAMAGED", 7, 9}, {"WAIT", 4, 8}},
		"no replica yet; a: attach an empty storage of the tenant as one")
	l.live = true
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return replicasPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		rs, _ := r.ref.(*v1.ReplicaStatus)
		d := detailScreen{base: base{c.newID()}, heading: "Replica " + clean(rs.GetReplica()), ref: rs, pairs: replicaPairs}
		return push(d, nil)
	}
	canAdmin := c.acc.onTenant(tenant, v1.Role_ROLE_ADMIN)
	canOperate := c.acc.onTenant(tenant, v1.Role_ROLE_OPERATOR)
	if canAdmin {
		l.actions = append(l.actions,
			action{key: "a", help: "attach", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newAttachReplica(c, tenant), nil) }},
			action{key: "d", help: "detach", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				rs, _ := ref.(*v1.ReplicaStatus)
				return push(newDetachReplica(c, tenant, rs.GetReplica()), nil)
			}},
			action{key: "r", help: "repair", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				rs, _ := ref.(*v1.ReplicaStatus)
				s, cmd := newRepairList(c, tenant, rs.GetPrimary())
				return push(s, cmd)
			}},
			action{key: "s", help: "switch", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				rs, _ := ref.(*v1.ReplicaStatus)
				s, cmd := newSwitchPreview(c, tenant, rs.GetPrimary(), rs.GetReplica())
				return push(s, cmd)
			}})
	}
	if canOperate {
		l.actions = append(l.actions, action{key: "b", help: "backfill", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			rs, _ := ref.(*v1.ReplicaStatus)
			return push(newBackfillReplica(c, tenant, rs.GetReplica()), nil)
		}})
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// replicaPairs are the lines of one replica's state.
func replicaPairs(c *ctx, ref any) []pair {
	r, _ := ref.(*v1.ReplicaStatus)
	ps := []pair{
		{"Replica", r.GetReplica()}, {"Primary", r.GetPrimary()}, {"Tier", output.StorageTier(r.GetTier())},
		{"In step", itoa(r.GetScansInStep()) + " scans"},
		{"Waiting", itoa(r.GetScansPending()) + " scans to be copied"},
		{"Being copied", itoa(r.GetScansCopying()) + " scans"},
		{"Damaged", itoa(r.GetScansDamaged()) + " scans, " + itoa(r.GetObjectsDamaged()) + " objects"},
	}
	if r.GetLag() != nil && r.GetLag().AsDuration() > 0 {
		ps = append(ps, pair{"Oldest wait", output.Span(r.GetLag().AsDuration())})
	}
	if r.GetSharesBucket() {
		ps = append(ps, pair{"Shares a bucket", "yes: with its primary, so it does not survive the loss of the bucket"})
	}
	return ps
}

// newAttachReplica asks which empty storage becomes a replica of which
// primary, and says what attaching does.
func newAttachReplica(c *ctx, tenant string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Attach a replica to " + clean(tenant),
		intro: []string{
			"The storage must hold no archived file and no object of the tenant, and no receiver or route may name it.",
			"The server then copies what the primary holds, and every scan archived from now on, behind the reception.",
			"The tier and recall settings of an archive-tier replica are set with \"nopacs replica attach --tier\".",
		},
		fields: []formField{{label: "Storage", hint: "the empty storage that becomes the replica"}, {label: "Primary", hint: "the storage it copies"}},
		verb:   "attach",
		pop:    1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if f.value(0) == "" || f.value(1) == "" {
			return nil, fault.New(fault.Invalid, "name the storage and its primary")
		}
		return attachReplica(c, f.id, tenant, f.value(0), f.value(1)), nil
	}
	return f
}

// newDetachReplica asks for the replica's name to be typed. Detaching deletes
// nothing, but one that abandons the purges that count the replica cannot be
// undone, so it asks for a reason as well.
func newDetachReplica(c *ctx, tenant, replica string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Detach replica " + clean(replica),
		intro: []string{
			"Copying to it stops and reads no longer turn to it. No file is deleted: they stay where they are.",
			"While a delete or a relabel still counts this replica among its copies the server refuses, unless the purges are abandoned:",
			"each such correction then records the replica's objects as unknown, and that cannot be undone.",
		},
		fields: []formField{
			{label: "Type the name", hint: replica},
			{label: "Abandon purges", choices: []string{"no", "yes"}},
			{label: "Reason", hint: "only with abandon: why, kept in the audit log"},
		},
		want: replica,
		verb: "detach",
		pop:  1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		abandon := f.value(1) == "yes"
		if abandon && f.value(2) == "" {
			return nil, fault.New(fault.Invalid, "say why the purges are abandoned")
		}
		reason := ""
		if abandon {
			reason = f.value(2)
		}
		return detachReplica(c, f.id, tenant, replica, abandon, reason), nil
	}
	return f
}

// newBackfillReplica asks before it queues a copy of everything the replica
// lacks: it changes nothing on the primary, but it costs reads and bandwidth.
func newBackfillReplica(c *ctx, tenant, replica string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Backfill replica " + clean(replica),
		intro: []string{
			"This queues a job that copies to the replica every object of its primary that it does not hold verified.",
			"A server that does not serve this yet says so, and nothing is changed.",
		},
		fields: []formField{{label: "Start the copy", choices: []string{"no", "yes"}}},
		verb:   "start",
		pop:    1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if f.value(0) != "yes" {
			return nil, fault.New(fault.Invalid, "choose yes to start the copy")
		}
		return backfillReplica(c, f.id, tenant, replica), nil
	}
	return f
}

// newRepairList lists the findings that hold on a primary and a repair mends;
// r repairs the one under the cursor and a all of them, each after the
// storage's name is typed.
func newRepairList(c *ctx, tenant, storage string) (screen, tea.Cmd) {
	l := newList(c, "Findings on "+clean(storage), []column{{"ID", 8, 26}, {"KIND", 6, 18}, {"STORAGE", 7, 40}, {"SCAN", 8, 60}, {"INSTANCE", 8, 26}, {"FOUND", 8, 19}},
		"no finding holds on this storage: nothing to repair")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return findingsPage(c, l.id, l.gen, token, tenant, storage, true)
	}
	l.actions = []action{
		{key: "r", help: "repair this", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			f, _ := ref.(*v1.ScrubFinding)
			return push(newRepairForm(c, tenant, storage, []string{f.GetId()}, false), nil)
		}},
		{key: "a", help: "repair all", do: func(c *ctx, _ screen, _ any) tea.Cmd {
			return push(newRepairForm(c, tenant, storage, nil, true), nil)
		}},
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newRepairForm asks for the primary's name to be typed before a repair: it
// writes to the primary, putting back the bytes the catalog names from a
// replica in step.
func newRepairForm(c *ctx, tenant, storage string, findings []string, all bool) screen {
	what := "the finding chosen"
	if all {
		what = "every finding that holds on the storage now, of a kind a repair mends"
	}
	f := formScreen{
		base:    base{c.newID()},
		heading: "Repair " + clean(storage),
		intro: []string{
			"This repairs " + what + ": each object is read whole from a replica in step, checked against the catalog's length and SHA-256,",
			"put back at the same key and read back; then its finding is cleared. An object no replica holds good is left as it is and counted refused.",
		},
		fields: []formField{{label: "Type the name", hint: storage}, {label: "From replica", hint: "empty: any replica in step"}},
		want:   storage,
		verb:   "repair",
		pop:    2,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		return repairStorage(c, f.id, tenant, storage, f.value(1), findings, all), nil
	}
	return f
}
