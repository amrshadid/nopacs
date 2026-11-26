package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newStorageStatus builds "storage status".
func (a *app) newStorageStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status <tenant> [<name>]",
		Short: "Show a tenant's storages now: space, tier, replicas, switch, encryption and scrub coverage",
		Long: `Show the state of a tenant's storages now, from the tenant's status. Without a
name it is one table of every storage; with a name it is that storage in full.

For each storage: the space it has used and has left; its tier (an archive-tier
replica may hold objects that must be recalled before they read); for a replica,
its primary and how far it is behind (the scans in step with the primary and the
scans still waiting to be copied); whether a switch of it is in progress (then
its receivers refuse new data for a moment, and every sender tries again) and
when the last one ended; whether it encrypts what it holds at rest, never more
strongly than the storage or its admin said (a directory shows the admin's
statement as "declared", which the server cannot check); and how much of it
scrubs have read whole within the tenant's scrub interval.

It needs the reader role on the tenant.`,
		Example: `  nopacs storage status lab1
  nopacs storage status lab1 cold
  nopacs storage status lab1 --json`,
		Args: cobra.RangeArgs(1, 2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetStatusResponse, error) {
				return c.Monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: optionalScope(args[0])})
			})
			if err != nil {
				return err
			}
			if len(res.GetTenants()) == 0 {
				return fault.New(fault.NotFound, fmt.Sprintf("the server answered no status for tenant %s", output.Clean(args[0])))
			}
			st := res.GetTenants()[0]
			if len(args) == 2 {
				for _, s := range st.GetStorages() {
					if s.GetStorage() == args[1] {
						return messageOut(a, s, func(w io.Writer) error { return a.storageStatusDetails(w, s, st.GetReplicas()) })
					}
				}
				return fault.New(fault.NotFound, fmt.Sprintf("the tenant has no storage named %s in its status", output.Clean(args[1])))
			}
			return messageOut(a, res, func(w io.Writer) error { return a.storageStatusTable(w, st) })
		}),
	}
}

// replicaLag finds the status of a replica of the tenant, nil when none.
func replicaLag(replicas []*v1.ReplicaStatus, name string) *v1.ReplicaStatus {
	for _, r := range replicas {
		if r.GetReplica() == name {
			return r
		}
	}
	return nil
}

// lagText says how far a replica is behind its primary.
func lagText(r *v1.ReplicaStatus) string {
	if r == nil {
		return "-"
	}
	s := fmt.Sprintf("%d scans in step, %d waiting", r.GetScansInStep(), r.GetScansPending())
	if r.GetScansCopying() > 0 {
		s += fmt.Sprintf(", %d being copied", r.GetScansCopying())
	}
	if r.GetScansDamaged() > 0 {
		s += fmt.Sprintf(", %d with damaged objects (%d objects)", r.GetScansDamaged(), r.GetObjectsDamaged())
	}
	if r.GetLag() != nil && r.GetLag().AsDuration() > 0 {
		s += ", behind by " + output.Span(r.GetLag().AsDuration())
	}
	return s
}

// storageStatusTable writes every storage of a tenant's status as one table.
func (a *app) storageStatusTable(w io.Writer, st *v1.TenantStatus) error {
	if len(st.GetStorages()) == 0 {
		_, err := fmt.Fprintln(w, "no storage")
		return err
	}
	rows := make([][]string, 0, len(st.GetStorages()))
	for _, s := range st.GetStorages() {
		free := "unknown"
		if s.FreeBytes != nil {
			free = output.Human(s.GetFreeBytes())
		}
		coverage := "-"
		if cv := s.GetCoverage(); cv != nil {
			coverage = fmt.Sprintf("%.0f%%", cv.GetShare()*100)
		}
		rows = append(rows, []string{
			s.GetStorage(), output.StorageTier(s.GetTier()), output.Dash(output.Clean(s.GetReplicaOf())),
			output.Human(s.GetUsedBytes()), free, yesNo(s.GetSwitching()),
			shortEncryption(s), coverage,
		})
	}
	return output.Table(w, a.style(), []string{"STORAGE", "TIER", "REPLICA OF", "USED", "FREE", "SWITCHING", "ENCRYPTED", "COVERAGE"}, rows)
}

// shortEncryption is the one word a table shows for encryption at rest.
func shortEncryption(s *v1.StorageStatus) string {
	switch s.GetEncryptionAtRest() {
	case v1.EncryptionAtRest_ENCRYPTION_AT_REST_ENCRYPTED:
		return "yes"
	case v1.EncryptionAtRest_ENCRYPTION_AT_REST_NONE:
		return "no"
	case v1.EncryptionAtRest_ENCRYPTION_AT_REST_DECLARED:
		return "declared"
	case v1.EncryptionAtRest_ENCRYPTION_AT_REST_NOT_DECLARED:
		return "not declared"
	}
	return "unknown"
}

// storageStatusDetails writes one storage's status in full.
func (a *app) storageStatusDetails(w io.Writer, s *v1.StorageStatus, replicas []*v1.ReplicaStatus) error {
	free := "unknown"
	if s.FreeBytes != nil {
		free = output.Human(s.GetFreeBytes())
	}
	capacity := "no limit of its own"
	if s.GetCapacityBytes() > 0 {
		capacity = output.Human(s.GetCapacityBytes())
	}
	switching := "no"
	if s.GetSwitching() {
		switching = "yes: its receivers refuse new data until it ends"
	}
	pairs := []output.Pair{
		{Label: "Storage", Value: s.GetStorage()},
		{Label: "Tier", Value: output.StorageTier(s.GetTier())},
		{Label: "Replica of", Value: output.Dash(output.Clean(s.GetReplicaOf()))},
	}
	if s.GetReplicaOf() != "" {
		pairs = append(pairs, output.Pair{Label: "Behind its primary", Value: lagText(replicaLag(replicas, s.GetStorage()))})
	}
	pairs = append(pairs,
		output.Pair{Label: "Used", Value: output.Human(s.GetUsedBytes())},
		output.Pair{Label: "Free", Value: free},
		output.Pair{Label: "Capacity", Value: capacity},
		output.Pair{Label: "Nearly full", Value: yesNo(s.GetNearlyFull())},
		output.Pair{Label: "Versioning", Value: enumWord(s.GetVersioning().String(), "STORAGE_VERSIONING_")},
		output.Pair{Label: "Object lock", Value: yesNo(s.GetObjectLock())},
		output.Pair{Label: "Switching", Value: switching},
		output.Pair{Label: "Last switch", Value: a.whenOr(s.GetLastSwitchTime(), "-")},
		output.Pair{Label: "Switch generation", Value: strconv.FormatInt(s.GetSwitchGeneration(), 10)},
		output.Pair{Label: "Encrypted at rest", Value: output.Encryption(s.GetEncryptionAtRest(), output.Clean(s.GetEncryptionAlgorithm()))},
		output.Pair{Label: "Scrub coverage", Value: output.Coverage(s.GetCoverage())})
	return output.Details(w, a.style(), pairs)
}
