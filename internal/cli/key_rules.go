package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// maxCertFile bounds what is read of a --client-cert file: a certificate in
// PEM is a few kilobytes, a chain a few more.
const maxCertFile = 64 << 10

// ruleFlags are the flags that say where a key may be used from: its
// networks, and the client certificate it must come with, as a PEM file or
// as its SHA-256.
type ruleFlags struct {
	networks []string
	certFile string
	certSum  string
}

// add adds the flags to cmd.
func (r *ruleFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringArrayVar(&r.networks, "network", nil, "a network the key may be used from, such as 10.0.0.0/8 or 192.0.2.7; give it once per network (default: every network)")
	cmd.Flags().StringVar(&r.certFile, "client-cert", "", "a PEM file of the TLS client certificate the key must come with")
	cmd.Flags().StringVar(&r.certSum, "client-cert-sha256", "", "the SHA-256 of the TLS client certificate the key must come with, in hexadecimal")
	cmd.MarkFlagsMutuallyExclusive("client-cert", "client-cert-sha256")
}

// read returns the networks and the certificate's fingerprint the flags
// give. The networks are sent as written, for the server to check; a
// certificate file is read here, its first certificate hashed, and only the
// hash is sent. A fingerprint written in capitals is lowered, as the server
// keeps it.
func (r *ruleFlags) read(_ *app) ([]string, string, error) {
	sum := strings.ToLower(strings.TrimSpace(r.certSum))
	if r.certFile != "" {
		f, err := os.Open(r.certFile) // #nosec G304 -- the path is the person's own --client-cert
		if err != nil {
			return nil, "", fault.Wrap(fault.Invalid, err, "cannot read --client-cert "+r.certFile)
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, maxCertFile+1))
		if err != nil {
			return nil, "", fault.Wrap(fault.Invalid, err, "cannot read --client-cert "+r.certFile)
		}
		if len(b) > maxCertFile {
			return nil, "", fault.New(fault.Invalid, "--client-cert "+r.certFile+" is larger than 64 KiB")
		}
		var block *pem.Block
		for rest := b; ; {
			block, rest = pem.Decode(rest)
			if block == nil || block.Type == "CERTIFICATE" {
				break
			}
		}
		if block == nil {
			return nil, "", fault.New(fault.Invalid, "--client-cert "+r.certFile+" holds no PEM certificate")
		}
		h := sha256.Sum256(block.Bytes)
		sum = hex.EncodeToString(h[:])
	}
	return r.networks, sum, nil
}

// newKeyRotate builds "key rotate".
func (a *app) newKeyRotate() *cobra.Command {
	var overlap string
	cmd := &cobra.Command{
		Use:   "rotate <id> [--overlap 24h]",
		Short: "Replace a key with a new one, with no gap",
		Long: `Replace a key with a new one, its successor, and show the successor once.

The successor holds the same grants, holder, networks and client certificate.
The old key keeps working for the overlap (a day unless --overlap says
otherwise, at most 30 days), then stops: whoever holds it has that long to
switch to the successor. The successor lives as long as the old key was made
to live, never longer than the installation's key.max-lifetime.

An admin on the installation can rotate any key; a key can rotate itself, so
a system can renew its own key: run this with that key in NOPACS_KEY or
--key-file and its own id. A key is rotated once; rotate its successor next
time.

On standard output the successor is alone on its line, as key create prints
a new key; with --json the whole answer is written there.`,
		Example: `  nopacs key rotate k8Zp2QvXn5Rt
  nopacs key rotate k8Zp2QvXn5Rt --overlap 7d`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkKeyID(args[0]); err != nil {
				return err
			}
			req := &v1.RotateKeyRequest{Id: args[0]}
			if overlap != "" {
				d, err := parseSpan(overlap)
				if err != nil {
					return fault.New(fault.Invalid, fmt.Sprintf("--overlap %q is not a length of time; use a number and s, m, h, d or w, such as 24h", output.Clean(overlap)))
				}
				req.Overlap = durationpb.New(d)
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RotateKeyResponse, error) {
				return c.Access.RotateKey(ctx, req)
			})
			if err != nil {
				return err
			}
			if a.opts.json {
				return messageOut(a, res, nil)
			}
			// The successor, alone on its line, on standard output: with key
			// create, the only places a secret is ever printed.
			if err := a.write(func(w io.Writer) error {
				_, err := fmt.Fprintln(w, res.GetSecret())
				return err
			}); err != nil {
				return err
			}
			k, old := res.GetKey(), res.GetRotated()
			_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: key %s replaces %s, which stops working %s; the new key expires %s\n",
				output.Clean(keyLabel(k)), output.Clean(old.GetId()), a.when(old.GetExpireTime()), a.whenOr(k.GetExpireTime(), "never"))
			_, _ = fmt.Fprintln(a.env.Stderr, "nopacs: this is the only time the new key is shown; it cannot be shown again, so store it now")
			return nil
		}),
	}
	cmd.Flags().StringVar(&overlap, "overlap", "", "how long the old key keeps working: a number and s, m, h, d or w, such as 24h, at most 30d (default 24h)")
	return cmd
}

// newKeyReinstate builds "key reinstate".
func (a *app) newKeyReinstate() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "reinstate (--all | <id>...)",
		Short: "Let keys a restore suspended work again",
		Long: `Let suspended keys work again: the keys named by id, or every suspended key
with --all.

A restore suspends every key it brings back, since a key revoked, or a grant
removed, after the backup was made would otherwise work again. A suspended key
is refused off the machine as if it were wrong; "nopacs key list" shows it as
suspended. Review the keys first, revoke those that should stay ended, and
reinstate the rest. A key named that is not suspended is left as it is.
Reinstating does not bring back a revoked or expired key: it stays refused.

Only the local operator may reinstate keys: run this on the machine, over its
local socket (--data-dir), where commands need no key. Any key is refused,
whatever its grants. Each key reinstated has its own entry in the audit log.
At most 1000 ids are named at once.

On standard output each key reinstated is listed, or with --all the count;
with --json the whole answer is written there.`,
		Example: `  nopacs key reinstate k8Zp2QvXn5Rt --data-dir /var/lib/nopacs
  nopacs key reinstate k8Zp2QvXn5Rt k3Hw9TbYm2Qa
  nopacs key reinstate --all`,
		Args: func(cmd *cobra.Command, args []string) error {
			switch {
			case all && len(args) > 0:
				return fault.New(fault.Invalid, "give key ids or --all, not both; usage: "+cmd.UseLine())
			case !all && len(args) == 0:
				return fault.New(fault.Invalid, "name the keys to reinstate, or give --all; usage: "+cmd.UseLine())
			case len(args) > maxReinstateIDs:
				return fault.New(fault.Invalid, fmt.Sprintf("at most %d keys are reinstated by id at once (got %d); reinstate them in parts, or give --all", maxReinstateIDs, len(args)))
			}
			return nil
		},
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			for _, id := range args {
				if err := checkKeyID(id); err != nil {
					return err
				}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ReinstateKeysResponse, error) {
				return c.Access.ReinstateKeys(ctx, &v1.ReinstateKeysRequest{KeyIds: args, All: all})
			})
			if err != nil {
				return err
			}
			if !a.opts.json && !all {
				back := map[string]bool{}
				for _, k := range res.GetKeys() {
					back[k.GetId()] = true
				}
				for _, id := range args {
					if !back[id] {
						_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: key %s was not suspended; it is left as it was\n", output.Clean(id))
						back[id] = true // an id given twice is told once
					}
				}
			}
			return messageOut(a, res, func(w io.Writer) error {
				if all {
					noun := "keys"
					if res.GetReinstated() == 1 {
						noun = "key"
					}
					_, err := fmt.Fprintf(w, "%d %s reinstated\n", res.GetReinstated(), noun)
					return err
				}
				for _, k := range res.GetKeys() {
					if _, err := fmt.Fprintf(w, "key %s reinstated (%s)\n", output.Clean(keyLabel(k)), a.keyStatus(k)); err != nil {
						return err
					}
				}
				return nil
			})
		}),
	}
	cmd.Flags().BoolVar(&all, "all", false, "reinstate every suspended key, once they have been reviewed")
	return cmd
}

// maxReinstateIDs is the most key ids one reinstatement names, as the API
// contract allows (ReinstateKeysRequest.key_ids).
const maxReinstateIDs = 1000

// newKeySet builds "key set".
func (a *app) newKeySet() *cobra.Command {
	var rules ruleFlags
	cmd := &cobra.Command{
		Use:   "set <id> [--network CIDR]... [--client-cert FILE | --client-cert-sha256 HEX]",
		Short: "Set where a key may be used from",
		Long: `Set where a key may be used from, replacing what it had: the networks given
with --network, once per network, and the TLS client certificate given with
--client-cert (a PEM file) or --client-cert-sha256. A request with the key
from anywhere else, or without that certificate, is refused as if the key
were wrong. With no flag the key may be used from anywhere, with no
certificate. The installation's and the tenant's networks still apply.`,
		Example: `  nopacs key set k8Zp2QvXn5Rt --network 10.20.0.0/16 --network 192.0.2.7
  nopacs key set k8Zp2QvXn5Rt --client-cert bridge.pem
  nopacs key set k8Zp2QvXn5Rt`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkKeyID(args[0]); err != nil {
				return err
			}
			nets, cert, err := rules.read(a)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SetKeyRulesResponse, error) {
				return c.Access.SetKeyRules(ctx, &v1.SetKeyRulesRequest{Id: args[0], Networks: nets, ClientCertSha256: cert})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.keyDetails(w, res.GetKey()) })
		}),
	}
	rules.add(cmd)
	return cmd
}
