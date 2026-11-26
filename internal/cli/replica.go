package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newReplica builds the replica noun.
func (a *app) newReplica() *cobra.Command {
	return group("replica", "List, attach, detach and fill a tenant's replicas",
		`A replica is a second copy of a primary storage's data, on another storage of
the same tenant. The server copies each scan that is archived, replaced or
relabeled on the primary to its replicas, behind the reception, so until a scan
is in step its only copy is the primary's. A read that finds the primary's
object damaged, missing or out of reach turns to a replica that is in step for
that scan, and the bytes from it are checked against the catalog as the
primary's are. A storage becomes a replica when it is added with --replica-of or
attached here; it must hold nothing yet. A replica of the archive tier (an S3
cold storage class) may hold objects that must be recalled before they read.

Every command takes the tenant first. Listing needs the reader role on the
tenant, backfilling the operator role, attaching and detaching the admin role.
"repair" puts a primary's damaged objects back from a replica, and "switch"
moves a tenant onto a replica.`,
		a.newReplicaList(), a.newReplicaAttach(), a.newReplicaDetach(), a.newReplicaBackfill())
}

// replicaRow is one replica as a table row.
func replicaRow(r *v1.ReplicaStatus) []string {
	return []string{output.Clean(r.GetReplica()), output.Clean(r.GetPrimary()), output.StorageTier(r.GetTier()),
		strconv.FormatInt(r.GetScansInStep(), 10), strconv.FormatInt(r.GetScansPending(), 10), strconv.FormatInt(r.GetScansDamaged(), 10), lagCell(r)}
}

// lagCell is how long a replica's oldest waiting scan has waited.
func lagCell(r *v1.ReplicaStatus) string {
	if r.GetLag() == nil || r.GetLag().AsDuration() <= 0 {
		return "-"
	}
	return output.Span(r.GetLag().AsDuration())
}

// newReplicaList builds "replica list".
func (a *app) newReplicaList() *cobra.Command {
	var primary string
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant> [--primary NAME]",
		Short: "List a tenant's replicas and how far each is from its primary",
		Long: `List a tenant's replicas, ordered by primary and then name, each with its tier,
the scans it holds in step with its primary, the scans waiting to be copied, the
scans with damaged objects and how long the oldest waiting scan has waited.`,
		Example: `  nopacs replica list lab1
  nopacs replica list lab1 --primary main --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			replicas, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.ReplicaStatus, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListReplicasResponse, error) {
					return c.Replicas.ListReplicas(ctx, &v1.ListReplicasRequest{Tenant: args[0], Primary: primary, PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetReplicas(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "replicas", replicas, more, func() error {
				rows := make([][]string, 0, len(replicas))
				for _, r := range replicas {
					rows = append(rows, replicaRow(r))
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"REPLICA", "PRIMARY", "TIER", "IN STEP", "WAITING", "DAMAGED", "OLDEST WAIT"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&primary, "primary", "", "only the replicas of this primary")
	l.add(cmd)
	return cmd
}

// startedJob writes the line that says a job was queued and how to follow it.
func (a *app) startedJob(w io.Writer, tenant string, j *v1.Job) error {
	if j == nil {
		_, err := fmt.Fprintln(w, "nothing to copy: no job was queued")
		return err
	}
	_, err := fmt.Fprintf(w, "queued job %s; follow it with: nopacs job watch %s %s\n", output.Clean(j.GetId()), output.Clean(tenant), output.Clean(j.GetId()))
	return err
}

// newReplicaAttach builds "replica attach".
func (a *app) newReplicaAttach() *cobra.Command {
	var primary, tier, recallTier string
	var recallDays int
	var wait bool
	cmd := &cobra.Command{
		Use:   "attach <tenant> <storage> --primary NAME [--tier online|archive] [--recall-days N] [--recall-tier standard|bulk|expedited] [--wait]",
		Short: "Make an empty storage a replica of a primary and copy what the primary holds",
		Long: `Make an existing storage of the tenant a replica of a primary, and queue a job
that copies what the primary holds; scans archived from then on are copied as
they come. The storage must hold no archived file and no object under the
tenant's keys, its place must not overlap the primary's, no receiver or route may
name it, and the primary must not itself be a replica or be of the archive tier.
Attaching a storage that is already a replica of that primary changes nothing and
answers its copy job, so a repeated command starts nothing more.

--tier, --recall-days and --recall-tier set the storage's tier and, for an
archive-tier S3 storage, how long a recalled object stays readable and how fast
the store brings it back; they are applied by a second call after the attach,
since a tier other than online is only for a replica. If that call is refused the
attach still stands, and the command says so.

--wait follows the copy job until it ends, as "job watch" does.`,
		Example: `  nopacs replica attach lab1 copy --primary main
  nopacs replica attach lab1 cold --primary main --tier archive --recall-days 14 --recall-tier bulk --wait`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			var st *v1.Storage
			var paths []string
			if cmd.Flags().Changed("tier") || cmd.Flags().Changed("recall-days") || cmd.Flags().Changed("recall-tier") {
				var err error
				if st, paths, err = replicaTierChange(cmd, args[1], tier, recallDays, recallTier); err != nil {
					return err
				}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			cursor := ""
			if wait {
				if cursor, err = a.feedCursor(ctx, c, cmd, args[0]); err != nil {
					return err
				}
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.AttachReplicaResponse, error) {
				return c.Replicas.AttachReplica(ctx, &v1.AttachReplicaRequest{Tenant: args[0], Replica: args[1], Primary: primary})
			})
			if err != nil {
				return err
			}
			out := proto3Attach(res)
			if st != nil {
				set, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SetStorageResponse, error) {
					return c.Storages.SetStorage(ctx, &v1.SetStorageRequest{Tenant: args[0], Storage: st, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
				})
				if err != nil {
					return fault.New(fault.KindOf(err), fmt.Sprintf("storage %s is now a replica of %s (and its copy job %s is queued), but its tier settings were refused: %s",
						output.Clean(args[1]), output.Clean(primary), output.Clean(res.GetBackfill().GetId()), fault.Message(err)))
				}
				out.Storage = set.GetStorage()
			}
			if wait && out.GetBackfill() != nil {
				if err := a.write(func(w io.Writer) error { return a.attachedText(w, args[0], out) }); err != nil {
					return err
				}
				return a.followJob(ctx, c, cmd, args[0], out.GetBackfill().GetId(), cursor)
			}
			return messageOut(a, out, func(w io.Writer) error { return a.attachedText(w, args[0], out) })
		}),
	}
	fl := cmd.Flags()
	fl.StringVar(&primary, "primary", "", "the name of the primary storage to copy (required)")
	fl.StringVar(&tier, "tier", "", "online or archive: set the replica's tier after the attach")
	fl.IntVar(&recallDays, "recall-days", 0, "s3, archive tier: how many days a recalled object stays readable (1 to 365)")
	fl.StringVar(&recallTier, "recall-tier", "", "s3, archive tier: standard, bulk or expedited")
	fl.BoolVar(&wait, "wait", false, `follow the copy job until it ends, as "job watch" does, with its exit codes`)
	_ = cmd.MarkFlagRequired("primary")
	_ = cmd.RegisterFlagCompletionFunc("tier", cobra.FixedCompletions(storageTierNames, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("recall-tier", cobra.FixedCompletions(recallTierNames, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

// proto3Attach copies an attach answer so the tier call can replace its
// storage without changing what the first call returned.
func proto3Attach(r *v1.AttachReplicaResponse) *v1.AttachReplicaResponse {
	return &v1.AttachReplicaResponse{Storage: r.GetStorage(), Backfill: r.GetBackfill()}
}

// replicaTierChange reads the tier flags of "replica attach" into the storage
// and mask a SetStorage call carries.
func replicaTierChange(cmd *cobra.Command, name, tier string, days int, recallTier string) (*v1.Storage, []string, error) {
	st := &v1.Storage{Name: name}
	var paths []string
	if cmd.Flags().Changed("tier") {
		t, err := parseStorageTier(tier)
		if err != nil {
			return nil, nil, err
		}
		st.Tier = t
		paths = append(paths, "tier")
	}
	if cmd.Flags().Changed("recall-days") || cmd.Flags().Changed("recall-tier") {
		s3 := &v1.S3Storage{}
		if cmd.Flags().Changed("recall-days") {
			s3.RecallDays = int32(days) // #nosec G115 -- the server bounds it to 1..365
			paths = append(paths, "s3.recall_days")
		}
		if cmd.Flags().Changed("recall-tier") {
			t, err := parseRecallTier(recallTier)
			if err != nil {
				return nil, nil, err
			}
			s3.RecallTier = t
			paths = append(paths, "s3.recall_tier")
		}
		st.Type = &v1.Storage_S3{S3: s3}
	}
	return st, paths, nil
}

// attachedText says what an attach did.
func (a *app) attachedText(w io.Writer, tenant string, res *v1.AttachReplicaResponse) error {
	st := res.GetStorage()
	if _, err := fmt.Fprintf(w, "storage %s is a replica of %s (tier %s)\n", output.Clean(st.GetName()), output.Clean(st.GetReplicaOf()), output.StorageTier(st.GetTier())); err != nil {
		return err
	}
	return a.startedJob(w, tenant, res.GetBackfill())
}

// newReplicaDetach builds "replica detach".
func (a *app) newReplicaDetach() *cobra.Command {
	var abandon, yes bool
	var reason string
	cmd := &cobra.Command{
		Use:   "detach <tenant> <replica> [--abandon-purges --reason TEXT] [--yes]",
		Short: "Make a replica an ordinary storage again; it deletes no file",
		Long: `Make a replica an ordinary storage again: copying to it stops, its copy jobs are
canceled, and reads no longer turn to it. It deletes nothing: the files stay
where they are, and since the catalog names none of them, "storage remove" may
then remove its configuration.

While a committed or purging correction counts the replica among its copies the
server refuses, since the bytes that correction withdraws would stay there
unseen. --abandon-purges --reason goes ahead: each such correction records the
replica's objects as unknown and names the replica among the copies it
abandoned, so it never reads as if that copy were removed. That cannot be
undone, so it asks for the replica's name to be typed again on a terminal (or
--yes); with no terminal it detaches nothing and exits 2.`,
		Example: `  nopacs replica detach lab1 copy
  nopacs replica detach lab1 copy --abandon-purges --reason "replica lost with its site" --yes`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if abandon != (reason != "") {
				return fault.New(fault.Invalid, "--abandon-purges and --reason go together: say why the purges are abandoned")
			}
			if abandon {
				if err := checkReason(reason); err != nil {
					return err
				}
				if err := a.confirmTyped(yes, args[1], "detached", fmt.Sprintf("detach %s and abandon the purges that count it: those corrections will record its objects as unknown", output.Clean(args[1]))); err != nil {
					return err
				}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DetachReplicaResponse, error) {
				return c.Replicas.DetachReplica(ctx, &v1.DetachReplicaRequest{Tenant: args[0], Replica: args[1], AbandonPurges: abandon, AbandonReason: reason})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "storage %s is an ordinary storage again; no file was deleted\n", output.Clean(res.GetStorage().GetName()))
				return err
			})
		}),
	}
	cmd.Flags().BoolVar(&abandon, "abandon-purges", false, "detach even while corrections count the replica among their copies; they record its objects as unknown")
	cmd.Flags().StringVar(&reason, "reason", "", "why the purges are abandoned, in your own words, kept in the audit entry (with --abandon-purges)")
	cmd.Flags().BoolVar(&yes, "yes", false, "with --abandon-purges: do not ask for the replica's name to be typed again")
	return cmd
}

// newReplicaBackfill builds "replica backfill".
func (a *app) newReplicaBackfill() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "backfill <tenant> <replica> [--wait]",
		Short: "Copy to a replica every object of its primary it does not hold verified",
		Long: `Queue a job that copies to a replica every object of its primary that the replica
does not hold verified: after the replica was restored from elsewhere, or to be
sure before a switch. A copy job of the replica already queued or running is
answered instead, so a repeated command starts nothing more.

A server that does not serve this yet answers so, and the command says "not
served" and exits 1; "replica attach" copies what the primary holds when a
storage is attached.`,
		Example: `  nopacs replica backfill lab1 copy
  nopacs replica backfill lab1 copy --wait`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			cursor := ""
			if wait {
				if cursor, err = a.feedCursor(ctx, c, cmd, args[0]); err != nil {
					return err
				}
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.BackfillReplicaResponse, error) {
				return c.Replicas.BackfillReplica(ctx, &v1.BackfillReplicaRequest{Tenant: args[0], Replica: args[1]})
			})
			if err != nil {
				return client.OrNotServed(what(cmd), err)
			}
			if wait {
				return a.followJob(ctx, c, cmd, args[0], res.GetJob().GetId(), cursor)
			}
			return messageOut(a, res, func(w io.Writer) error { return a.startedJob(w, args[0], res.GetJob()) })
		}),
	}
	cmd.Flags().BoolVar(&wait, "wait", false, `follow the copy job until it ends, as "job watch" does, with its exit codes`)
	return cmd
}
