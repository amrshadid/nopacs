package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// This file holds the calls of the replica, repair and switch screens. Like
// every call here, each returns a command and none is made by a screen
// itself; the names given to client.Call are the commands' own.

// switchPreviewMsg is the answer of a switch's preview.
type switchPreviewMsg struct{ res *v1.SwitchStorageResponse }

// switchDoneMsg is the answer of a switch that was made.
type switchDoneMsg struct{ res *v1.SwitchStorageResponse }

// replicaRow is a replica as the cells of a row.
func replicaRow(r *v1.ReplicaStatus) row {
	wait := "-"
	if r.GetLag() != nil && r.GetLag().AsDuration() > 0 {
		wait = output.Span(r.GetLag().AsDuration())
	}
	return row{cells: []string{r.GetReplica(), r.GetPrimary(), output.StorageTier(r.GetTier()), itoa(r.GetScansInStep()),
		itoa(r.GetScansPending()), itoa(r.GetScansDamaged()), wait}, ref: r}
}

// replicasPage asks for one page of a tenant's replicas.
func replicasPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "replica list", func(ctx context.Context) (*v1.ListReplicasResponse, error) {
			return cl.Replicas.ListReplicas(ctx, &v1.ListReplicasRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListReplicasResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetReplicas()))
			for _, rs := range r.GetReplicas() {
				rows = append(rows, replicaRow(rs))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// attachReplica makes a storage a replica of a primary.
func attachReplica(c *ctx, owner int, tenant, replica, primary string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "replica attach", func(ctx context.Context) (*v1.AttachReplicaResponse, error) {
			return cl.Replicas.AttachReplica(ctx, &v1.AttachReplicaRequest{Tenant: tenant, Replica: replica, Primary: primary})
		})
		return answer(res, err, func(r *v1.AttachReplicaResponse) tea.Msg {
			note := "storage " + r.GetStorage().GetName() + " is a replica of " + r.GetStorage().GetReplicaOf()
			if j := r.GetBackfill(); j != nil {
				note += "; copy job " + j.GetId() + " queued"
			}
			return actionDoneMsg{note: note}
		})
	})
}

// detachReplica makes a replica an ordinary storage again.
func detachReplica(c *ctx, owner int, tenant, replica string, abandon bool, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "replica detach", func(ctx context.Context) (*v1.DetachReplicaResponse, error) {
			return cl.Replicas.DetachReplica(ctx, &v1.DetachReplicaRequest{Tenant: tenant, Replica: replica, AbandonPurges: abandon, AbandonReason: reason})
		})
		return answer(res, err, func(r *v1.DetachReplicaResponse) tea.Msg {
			return actionDoneMsg{note: "storage " + r.GetStorage().GetName() + " is an ordinary storage again; no file was deleted"}
		})
	})
}

// backfillReplica queues a copy of everything the replica lacks. A server that
// does not serve it yet says so in one plain line.
func backfillReplica(c *ctx, owner int, tenant, replica string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "replica backfill", func(ctx context.Context) (*v1.BackfillReplicaResponse, error) {
			return cl.Replicas.BackfillReplica(ctx, &v1.BackfillReplicaRequest{Tenant: tenant, Replica: replica})
		})
		if err != nil {
			return failMsg{client.OrNotServed("replica backfill", err)}
		}
		return actionDoneMsg{note: "copy job " + res.GetJob().GetId() + " queued"}
	})
}

// repairKinds are the kinds of finding a repair mends: an orphan is an object
// the catalog does not name, which a replica cannot put back.
var repairKinds = []v1.ScrubFindingKind{
	v1.ScrubFindingKind_SCRUB_FINDING_KIND_MISSING, v1.ScrubFindingKind_SCRUB_FINDING_KIND_DAMAGED,
	v1.ScrubFindingKind_SCRUB_FINDING_KIND_DISAGREEING, v1.ScrubFindingKind_SCRUB_FINDING_KIND_MANIFEST_DAMAGED,
}

// findingsPage asks for one page of the scrub findings that hold on a
// storage (every storage when empty): of the kinds a repair mends, or of every
// kind.
func findingsPage(c *ctx, owner, gen int, token, tenant, storage string, mendable bool) tea.Cmd {
	cl, loc := c.client, c.loc
	var kinds []v1.ScrubFindingKind
	if mendable {
		kinds = repairKinds
	}
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "job findings", func(ctx context.Context) (*v1.ListScrubFindingsResponse, error) {
			return cl.Corrections.ListScrubFindings(ctx, &v1.ListScrubFindingsRequest{Tenant: tenant, Storage: storage, Kinds: kinds,
				PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListScrubFindingsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetFindings()))
			for _, f := range r.GetFindings() {
				rows = append(rows, row{cells: []string{f.GetId(), output.FindingKind(f.GetKind()), f.GetStorage(), f.GetSubjectLabel() + "/" + f.GetSessionLabel() + "/" + f.GetScanLabel(),
					output.Dash(f.GetInstanceId()), listTime(f.GetFindTime(), loc)}, ref: f})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// repairStorage queues a repair of a primary's findings, all of them or the
// ones named.
func repairStorage(c *ctx, owner int, tenant, storage, from string, findings []string, all bool) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "repair", func(ctx context.Context) (*v1.RepairResponse, error) {
			return cl.Replicas.Repair(ctx, &v1.RepairRequest{Tenant: tenant, Storage: storage, From: from, Findings: findings, All: all})
		})
		return answer(res, err, func(r *v1.RepairResponse) tea.Msg {
			return actionDoneMsg{note: "repair job " + r.GetJob().GetId() + " queued"}
		})
	})
}

// previewSwitch asks whether a switch could start now, what stands in its way
// and for the token it must carry; nothing changes.
func previewSwitch(c *ctx, owner int, tenant, primary, replica string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "switch", func(ctx context.Context) (*v1.SwitchStorageResponse, error) {
			return cl.Replicas.SwitchStorage(ctx, &v1.SwitchStorageRequest{Tenant: tenant, Primary: primary, Replica: replica, ValidateOnly: true})
		})
		return answer(res, err, func(r *v1.SwitchStorageResponse) tea.Msg { return switchPreviewMsg{res: r} })
	})
}

// switchStorage makes the switch with the token a preview gave. A switch that
// was accepted runs on in the server whatever becomes of this call.
func switchStorage(c *ctx, owner int, tenant, primary, replica, token string, skip bool, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "switch", func(ctx context.Context) (*v1.SwitchStorageResponse, error) {
			return cl.Replicas.SwitchStorage(ctx, &v1.SwitchStorageRequest{Tenant: tenant, Primary: primary, Replica: replica,
				Confirm: token, SkipVerification: skip, SkipReason: reason})
		})
		return answer(res, err, func(r *v1.SwitchStorageResponse) tea.Msg { return switchDoneMsg{res: r} })
	})
}
