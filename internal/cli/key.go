package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// keyLabel is a key's id with its holder, for a sentence.
func keyLabel(k *v1.Key) string { return output.KeyLabel(k) }

// keyStatus says whether a key works now: revoked, expired, or active.
func (a *app) keyStatus(k *v1.Key) string { return output.KeyStatus(k, a.env.Now()) }

// newKey builds the key noun.
func (a *app) newKey() *cobra.Command {
	return group("key", "Create, list, show, rotate, restrict, revoke and reinstate keys",
		`A key is a credential held by a person or a system. A key has no rights of its
own: grants give it roles (see "nopacs grant"). These commands need the admin
role on the installation, but a key may rotate itself, and only the local
operator may reinstate the keys a restore suspended. A key's secret is shown
once, when it is made or rotated, and cannot be shown again.`,
		a.newKeyCreate(), a.newKeyList(), a.newKeyShow(), a.newKeyRevoke(), a.newKeyRotate(), a.newKeySet(), a.newKeyReinstate())
}

// keyDetails writes one key as labeled lines: its rules and its successor
// only when it has them.
func (a *app) keyDetails(w io.Writer, k *v1.Key) error {
	pairs := []output.Pair{
		{Label: "ID", Value: k.GetId()},
		{Label: "Holder", Value: k.GetHolder()},
		{Label: "Status", Value: a.keyStatus(k)},
		{Label: "Created", Value: a.when(k.GetCreateTime())},
		{Label: "Expires", Value: a.whenOr(k.GetExpireTime(), "never")},
		{Label: "Last used", Value: a.lastUse(k)},
		{Label: "Revoked", Value: a.whenOr(k.GetRevokeTime(), "-")},
	}
	if n := k.GetNetworks(); len(n) > 0 {
		pairs = append(pairs, output.Pair{Label: "Networks", Value: strings.Join(n, ", ")})
	}
	if c := k.GetClientCertSha256(); c != "" {
		pairs = append(pairs, output.Pair{Label: "Client cert", Value: "sha256 " + c})
	}
	if s := k.GetSuccessorId(); s != "" {
		pairs = append(pairs, output.Pair{Label: "Successor", Value: s})
	}
	if k.GetSuspended() {
		pairs = append(pairs, output.Pair{Label: "Suspended", Value: a.whenOr(k.GetSuspendTime(), "yes")})
	}
	return output.Details(w, a.style(), pairs)
}

// lastUse is a key's last use with the address it came from, when known.
func (a *app) lastUse(k *v1.Key) string {
	v := a.whenOr(k.GetLastUseTime(), "never")
	if addr := k.GetLastUseAddress(); addr != "" && k.GetLastUseTime() != nil {
		v += " from " + addr
	}
	return v
}

// newKeyCreate builds "key create".
func (a *app) newKeyCreate() *cobra.Command {
	var holder, expires string
	var noExpiry bool
	var rules ruleFlags
	cmd := &cobra.Command{
		Use:   "create --holder TEXT [--expires 90d | --no-expiry] [--network CIDR]... [--client-cert FILE | --client-cert-sha256 HEX]",
		Short: "Make a key and show it once",
		Long: `Make a key for a person or a system and show it once.

On standard output the key is alone on its line, so a script can capture it
(KEY=$(nopacs key create --holder ci)). On standard error a note says that it
will not be shown again: the server keeps only a hash of it and cannot show it
a second time. Store it now, in a file only its holder can read, and give it
to a command through NOPACS_KEY or --key-file.

A key expires after 90 days unless --expires says otherwise, and never later
than the installation's key.max-lifetime; a key that never expires has to be
asked for with --no-expiry, which only an admin on the installation can do.
--network, given once per network, limits where the key may be used from;
--client-cert (a PEM file) or --client-cert-sha256 makes it work only with
that TLS client certificate. With --json the whole answer, the key included,
is written to standard output.`,
		Example: `  nopacs key create --holder "nightly export"
  nopacs key create --holder ci --expires 30d
  nopacs key create --holder service-x --no-expiry
  nopacs key create --holder scanner-bridge --network 10.20.0.0/16 --client-cert bridge.pem`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			req := &v1.CreateKeyRequest{Holder: holder}
			nets, cert, err := rules.read(a)
			if err != nil {
				return err
			}
			req.Networks, req.ClientCertSha256 = nets, cert
			switch {
			case noExpiry:
				req.Expiry = &v1.CreateKeyRequest_NeverExpire{NeverExpire: true}
			case expires != "":
				d, err := parseSpan(expires)
				if err != nil {
					return fault.New(fault.Invalid, fmt.Sprintf("--expires %q is not a length of time; use a number and s, m, h, d or w, such as 90d", output.Clean(expires)))
				}
				req.Expiry = &v1.CreateKeyRequest_ExpireAfter{ExpireAfter: durationpb.New(d)}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CreateKeyResponse, error) {
				return c.Access.CreateKey(ctx, req)
			})
			if err != nil {
				return err
			}
			if a.opts.json {
				return messageOut(a, res, nil)
			}
			// The key, alone on its line, on standard output: the only place
			// a secret is ever printed.
			if err := a.write(func(w io.Writer) error {
				_, err := fmt.Fprintln(w, res.GetSecret())
				return err
			}); err != nil {
				return err
			}
			k := res.GetKey()
			_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: key %s, expires %s\n", output.Clean(keyLabel(k)), a.whenOr(k.GetExpireTime(), "never"))
			_, _ = fmt.Fprintln(a.env.Stderr, "nopacs: this is the only time the key is shown; it cannot be shown again, so store it now")
			return nil
		}),
	}
	cmd.Flags().StringVar(&holder, "holder", "", "who holds the key: a person or a system, at most 256 bytes (required)")
	cmd.Flags().StringVar(&expires, "expires", "", "how long the key works: a number and s, m, h, d or w, such as 90d (default: the installation's setting key.default-lifetime, 90 days unless set)")
	cmd.Flags().BoolVar(&noExpiry, "no-expiry", false, "make a key that never expires (admin on the installation only)")
	rules.add(cmd)
	_ = cmd.MarkFlagRequired("holder")
	cmd.MarkFlagsMutuallyExclusive("expires", "no-expiry")
	return cmd
}

// newKeyList builds "key list".
func (a *app) newKeyList() *cobra.Command {
	var l listFlags
	var tenant string
	cmd := &cobra.Command{
		Use:   "list [--tenant NAME]",
		Short: "List every key, revoked and expired ones included, or the keys of one tenant",
		Long: `List the installation's keys, oldest first, with whether each is active,
expired or revoked. The secrets are not here: the server never has them.

With --tenant only the keys that hold a grant on that tenant are listed, in
order of key id, and a tenant's admin may ask for them: it is how it finds the
key to give a role to. Such a listing shows each key's id and holder only;
"grant list --tenant" says their roles.`,
		Example: `  nopacs key list
  nopacs key list --json
  nopacs key list --tenant lab1`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			keys, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Key, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListKeysResponse, error) {
					return c.Access.ListKeys(ctx, &v1.ListKeysRequest{PageSize: size, PageToken: token, Tenant: tenant})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetKeys(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "keys", keys, more, func() error {
				if tenant != "" {
					rows := make([][]string, 0, len(keys))
					for _, k := range keys {
						rows = append(rows, []string{k.GetId(), k.GetHolder()})
					}
					return a.write(func(w io.Writer) error { return output.Table(w, a.style(), []string{"ID", "HOLDER"}, rows) })
				}
				rows := make([][]string, 0, len(keys))
				for _, k := range keys {
					rows = append(rows, []string{k.GetId(), k.GetHolder(), a.keyStatus(k), a.when(k.GetCreateTime()), a.whenOr(k.GetExpireTime(), "never"), a.lastUse(k)})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"ID", "HOLDER", "STATUS", "CREATED", "EXPIRES", "LAST USED"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "list only the keys that hold a grant on this tenant")
	l.add(cmd)
	return cmd
}

// newKeyShow builds "key show".
func (a *app) newKeyShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one key",
		Long: `Show one key by its id: its holder, whether it works, when it was made, when it
expires, when it was last used and when it was revoked. The id is public; it is
the part of a key between "nopacs_" and the second underscore.`,
		Example: `  nopacs key show k8Zp2QvXn5Rt`,
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkKeyID(args[0]); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetKeyResponse, error) {
				return c.Access.GetKey(ctx, &v1.GetKeyRequest{Id: args[0]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.keyDetails(w, res.GetKey()) })
		}),
	}
}

// newKeyRevoke builds "key revoke".
func (a *app) newKeyRevoke() *cobra.Command {
	var withSuccessors bool
	cmd := &cobra.Command{
		Use:   "revoke <id> [--with-successors]",
		Short: "End a key for good",
		Long: `Revoke a key: from this moment a request with it is refused. Its grants stay
listed, so the record shows what it could do, but they give nothing. Revoking a
key that is already revoked changes nothing and shows its first revoke time.
There is no way to undo it; make a new key instead.

Revoking a rotated key does not revoke the key that replaced it: ending the old
key early, once its holder has switched, is the common case. When a successor
made by rotation still works, revoke says so. --with-successors revokes the
whole chain the key began: its successor, that key's successor, and so on,
each with its own entry in the audit log, as a leaked key calls for.`,
		Example: `  nopacs key revoke k8Zp2QvXn5Rt
  nopacs key revoke k8Zp2QvXn5Rt --with-successors`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkKeyID(args[0]); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			revoke := func(id string) (*v1.Key, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RevokeKeyResponse, error) {
					return c.Access.RevokeKey(ctx, &v1.RevokeKeyRequest{Id: id})
				})
				return res.GetKey(), err
			}
			first, err := revoke(args[0])
			if err != nil {
				return err
			}
			revoked := []*v1.Key{first}
			var live []*v1.Key
			seen := map[string]bool{first.GetId(): true}
			for next := first.GetSuccessorId(); next != "" && !seen[next] && len(seen) <= maxSuccessorChain; {
				seen[next] = true
				var k *v1.Key
				if withSuccessors {
					k, err = revoke(next)
					if err == nil {
						revoked = append(revoked, k)
					}
				} else {
					var res *v1.GetKeyResponse
					res, err = call(ctx, c, cmd, func(ctx context.Context) (*v1.GetKeyResponse, error) {
						return c.Access.GetKey(ctx, &v1.GetKeyRequest{Id: next})
					})
					k = res.GetKey()
					if err == nil && a.keyStatus(k) == "active" {
						live = append(live, k)
					}
				}
				if err != nil {
					// What was revoked stays revoked: said before the error, so
					// a failure further along the chain does not read as if
					// nothing had happened.
					for _, k := range revoked {
						_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: key %s was revoked at %s\n", output.Clean(keyLabel(k)), a.when(k.GetRevokeTime()))
					}
					return err
				}
				next = k.GetSuccessorId()
			}
			if a.opts.json && withSuccessors {
				return a.write(func(w io.Writer) error { return output.JSONList(w, "keys", revoked, false) })
			}
			for _, k := range live {
				_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: key %s, made from it by rotation, still works; to end it too: nopacs key revoke %s --with-successors\n",
					output.Clean(keyLabel(k)), first.GetId())
			}
			return messageOut(a, &v1.RevokeKeyResponse{Key: first}, func(w io.Writer) error {
				for _, k := range revoked {
					if _, err := fmt.Fprintf(w, "key %s revoked at %s\n", output.Clean(keyLabel(k)), a.when(k.GetRevokeTime())); err != nil {
						return err
					}
				}
				return nil
			})
		}),
	}
	cmd.Flags().BoolVar(&withSuccessors, "with-successors", false, "also revoke every key made from this one by rotation, in turn")
	return cmd
}

// maxSuccessorChain bounds how many successors revoke follows: each rotation
// makes one, and far fewer than this are made in a key line's life.
const maxSuccessorChain = 1000
