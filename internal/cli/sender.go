package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newSender builds the sender noun.
func (a *app) newSender() *cobra.Command {
	return group("sender", "Add, list, show, change, remove and test a tenant's senders",
		`A sender is a way for archived data to leave a tenant: one named destination,
the peer (a PACS, an Orthanc, another archive), and how to reach it. A dimse
sender opens associations with a DICOM peer and sends each instance by C-STORE;
a stow sender stores instances into a DICOMweb service by STOW-RS. Both send the
archived bytes as they are stored. A sender sends nothing by itself: a job asks
for a subject, a session or a scan to be sent (nopacs job create), or a receiver
that lists the sender in its forward-to sends what each reception archives.
Where a sender may send is the installation's to decide (the settings
sender.destinations, sender.allow-plain-http), since a sender moves patient data
out of the archive.
Every command takes the tenant first. Adding, changing, removing and testing
need the admin role on the tenant; list and show need the reader role.`,
		a.newSenderAdd(), a.newSenderList(), a.newSenderShow(), a.newSenderSet(), a.newSenderRemove(), a.newSenderTest())
}

// newSenderAdd builds "sender add".
func (a *app) newSenderAdd() *cobra.Command {
	var f senderFlags
	cmd := &cobra.Command{
		Use:   "add <tenant> <name> --type dimse --host H --port N --called-aet T --calling-aet T [--enabled] | --type stow --url URL --authentication KIND [--credential-file F] [--enabled]",
		Short: "Add a sender to a tenant",
		Long: `Add a sender to a tenant. Adding connects to nothing: "sender test" tries the
peer. A sender is added stopped (--enabled turns it on), and its jobs wait while
it is stopped, using none of their attempts.

A dimse sender needs --host, --port, --called-aet (the peer's AE title) and
--calling-aet (the title the server calls with, which the peer commonly admits
by). Without --tls-ca-file its associations are plain TCP and every instance
crosses the network unencrypted, which is fit only for a network the site trusts.

A stow sender needs --url (the base of the peer's DICOMweb service; requests go
to <url>/studies) and --authentication, stated in so many words: none, basic
(with --username), bearer, or header (with --header-name). Any but none needs
the credential: the password, the token or the header's value. It is a secret,
so it is never an argument; give it from a file readable only by you
(--credential-file), from standard input (--credential-stdin) or from an
environment variable (--credential-env). The server seals it and never answers
it.

The host and port, or the URL, must be allowed by the installation's setting
sender.destinations, and an http URL by sender.allow-plain-http; otherwise the
server refuses the sender (exit 5).

The answer says whether the sender is up, down or not yet connected to.`,
		Example: `  nopacs sender add lab1 pacs --type dimse --host pacs.lab.example --port 104 \
      --called-aet ORTHANC --calling-aet NOPACS --enabled
  printf '%s' "$TOKEN" | nopacs sender add lab1 archive2 --type stow \
      --url https://pacs2.lab.example/dicom-web --authentication bearer --credential-stdin
  nopacs sender add lab1 orthanc --type stow --url https://orthanc.lab.example/dicom-web \
      --authentication basic --username nopacs --credential-file ./orthanc.pass`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			s, _, err := f.sender(cmd, args[1], "")
			if err != nil {
				return err
			}
			if s.GetStow() != nil && f.credentialGiven(cmd) {
				cred, err := a.readStowCredential(&f)
				if err != nil {
					return err
				}
				setCredential(s, cred)
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.AddSenderResponse, error) {
				return c.Senders.AddSender(ctx, &v1.AddSenderRequest{Tenant: args[0], Sender: s})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.senderDetails(w, res.GetSender()) })
		}),
	}
	f.add(cmd, false)
	return cmd
}

// yesNo is a bool as a word.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// senderDetails writes one sender as labeled lines. The credential is never
// one of them: the server does not answer it, only whether one is held.
func (a *app) senderDetails(w io.Writer, s *v1.Sender) error {
	pairs := []output.Pair{
		{Label: "Name", Value: s.GetName()},
		{Label: "Type", Value: output.SenderType(s)},
		{Label: "Enabled", Value: yesNo(s.GetEnabled())},
		{Label: "Health", Value: output.SenderHealth(s.GetHealth())},
	}
	if h := s.GetHealth(); h.GetSince() != nil {
		pairs = append(pairs, output.Pair{Label: "Health since", Value: a.when(h.GetSince())})
	}
	if h := s.GetHealth(); h.GetNextTryTime() != nil {
		pairs = append(pairs, output.Pair{Label: "Next try", Value: a.when(h.GetNextTryTime())})
	}
	if h := s.GetHealth(); h.GetLastSuccessTime() != nil {
		pairs = append(pairs, output.Pair{Label: "Last success", Value: a.when(h.GetLastSuccessTime())})
	}
	tlsText := func(t *v1.SenderTls, dflt string) string {
		if t == nil {
			return dflt
		}
		text := "CA file " + output.Dash(t.GetCaFile())
		if t.GetCertificateFile() != "" {
			text += ", certificate " + t.GetCertificateFile() + ", private key " + t.GetPrivateKeyFile()
		}
		if t.GetServerName() != "" {
			text += ", server name " + t.GetServerName()
		}
		return text
	}
	if d := s.GetDimse(); d != nil {
		pairs = append(pairs,
			output.Pair{Label: "Host", Value: d.GetHost()},
			output.Pair{Label: "Port", Value: strconv.Itoa(int(d.GetPort()))},
			output.Pair{Label: "Called AE title", Value: d.GetCalledAeTitle()},
			output.Pair{Label: "Calling AE title", Value: d.GetCallingAeTitle()},
			output.Pair{Label: "TLS", Value: tlsText(d.GetTls(), "none (plain TCP)")})
	}
	if st := s.GetStow(); st != nil {
		auth := enumWordAuth(st.GetAuthentication())
		pairs = append(pairs,
			output.Pair{Label: "URL", Value: st.GetUrl()},
			output.Pair{Label: "Authentication", Value: auth})
		if st.GetUsername() != "" {
			pairs = append(pairs, output.Pair{Label: "Username", Value: st.GetUsername()})
		}
		if st.GetHeaderName() != "" {
			pairs = append(pairs, output.Pair{Label: "Header name", Value: st.GetHeaderName()})
		}
		pairs = append(pairs, output.Pair{Label: "Credential held", Value: yesNo(st.GetCredentialSet())})
		// A plain http URL has no TLS to describe: a line naming roots
		// would say it is checked when nothing is.
		if !strings.HasPrefix(strings.ToLower(st.GetUrl()), "http://") {
			pairs = append(pairs, output.Pair{Label: "TLS", Value: tlsText(st.GetTls(), "the system's roots")})
		}
		pairs = append(pairs,
			output.Pair{Label: "Max instances per request", Value: strconv.Itoa(int(st.GetMaxInstancesPerRequest()))},
			output.Pair{Label: "Max request size", Value: output.Bytes(st.GetMaxRequestBytes())})
	}
	rate := "no bound of its own"
	if s.GetMaxBytesPerSecond() > 0 {
		rate = output.Bytes(s.GetMaxBytesPerSecond()) + "/s"
	}
	pairs = append(pairs,
		output.Pair{Label: "Max associations", Value: strconv.Itoa(int(s.GetMaxAssociations()))},
		output.Pair{Label: "Rate bound", Value: rate},
		output.Pair{Label: "Connect timeout", Value: spanText(s.GetConnectTimeout())},
		output.Pair{Label: "Response timeout", Value: spanText(s.GetResponseTimeout())},
		output.Pair{Label: "Created", Value: a.when(s.GetCreateTime())},
		output.Pair{Label: "Updated", Value: a.when(s.GetUpdateTime())})
	return output.Details(w, a.style(), pairs)
}

// enumWordAuth is a stow authentication as the word the flag takes.
func enumWordAuth(a v1.StowAuthentication) string {
	names := enumNames(v1.StowAuthentication_name, stowAuthPrefix)
	if int(a) >= 1 && int(a) <= len(names) {
		return names[a-1]
	}
	return "unknown"
}

// newSenderList builds "sender list".
func (a *app) newSenderList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant>",
		Short: "List a tenant's senders with their health",
		Long: `List a tenant's senders ordered by name, each with whether it reaches its peer
now: up, unknown before the first connection, or down with the reason (disabled,
the peer failing, and so on). A removed sender is not listed.`,
		Example: `  nopacs sender list lab1
  nopacs sender list lab1 --json`,
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
			senders, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Sender, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListSendersResponse, error) {
					return c.Senders.ListSenders(ctx, &v1.ListSendersRequest{Tenant: args[0], PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetSenders(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "senders", senders, more, func() error {
				rows := make([][]string, 0, len(senders))
				for _, s := range senders {
					rows = append(rows, []string{s.GetName(), output.SenderType(s), output.SenderWhere(s), yesNo(s.GetEnabled()), output.SenderHealth(s.GetHealth())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"NAME", "TYPE", "TO", "ENABLED", "HEALTH"}, rows)
				})
			})
		}),
	}
	l.add(cmd)
	return cmd
}

// newSenderShow builds "sender show".
func (a *app) newSenderShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <tenant> <name>",
		Short: "Show one sender with its health now",
		Long: `Show one sender of a tenant: where it sends, how it proves who sends, its TLS
files and limits, and whether it reaches its peer now. The credential of a stow
sender is never shown; the output says only whether one is held.`,
		Example: `  nopacs sender show lab1 pacs`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetSenderResponse, error) {
				return c.Senders.GetSender(ctx, &v1.GetSenderRequest{Tenant: args[0], Name: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.senderDetails(w, res.GetSender()) })
		}),
	}
}

// newSenderSet builds "sender set".
func (a *app) newSenderSet() *cobra.Command {
	var f senderFlags
	cmd := &cobra.Command{
		Use:   "set <tenant> <name> [flags of the fields to change]",
		Short: "Change some fields of a sender",
		Long: `Change the fields of a sender that the flags name, and no other. A change
applies to the next association or request; one open goes on under the
configuration it was opened with. A change of the destination (the host, port,
URL or TLS) makes the sender's health unknown until its next connection. A
sender's type does not change.

A stow sender's credential is kept unless --credential-file, --credential-stdin
or --credential-env gives a new one. It is never sent to a place it was not given
for: --url or --authentication must come with the credential, unless the
authentication becomes none, which removes the held one. --enabled=false stops a
sender without removing it. --no-tls turns TLS off.`,
		Example: `  nopacs sender set lab1 pacs --enabled=false
  nopacs sender set lab1 pacs --max-associations 4 --max-bytes-per-second 20MiB
  nopacs sender set lab1 archive2 --url https://pacs3.lab.example/dicom-web --credential-file ./token`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			// The TLS flags are the same for both types; only the stored
			// sender says which one's TLS they change.
			typ := ""
			if _, known, _ := f.isStow(cmd); !known && (f.tls(cmd) != nil || f.noTLS) {
				got, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetSenderResponse, error) {
					return c.Senders.GetSender(ctx, &v1.GetSenderRequest{Tenant: args[0], Name: args[1]})
				})
				if err != nil {
					return err
				}
				typ = output.SenderType(got.GetSender())
			}
			s, paths, err := f.sender(cmd, args[1], typ)
			if err != nil {
				return err
			}
			if f.credentialGiven(cmd) {
				cred, err := a.readStowCredential(&f)
				if err != nil {
					return err
				}
				if s.GetStow() == nil {
					return fault.New(fault.Invalid, "a credential is a stow sender's: this sender is dimse")
				}
				setCredential(s, cred)
				paths = append(paths, "stow.credential")
			}
			if len(paths) == 0 {
				return fault.New(fault.Invalid, "nothing to change: give at least one flag, such as --enabled=false")
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SetSenderResponse, error) {
				return c.Senders.SetSender(ctx, &v1.SetSenderRequest{Tenant: args[0], Sender: s, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.senderDetails(w, res.GetSender()) })
		}),
	}
	f.add(cmd, true)
	return cmd
}

// newSenderRemove builds "sender remove".
func (a *app) newSenderRemove() *cobra.Command {
	var yes, cancelJobs bool
	cmd := &cobra.Command{
		Use:   "remove <tenant> <name> --yes [--cancel-jobs]",
		Short: "Remove a sender's configuration and its sealed credential",
		Long: `Remove a sender's configuration and the credential the server holds for it. It
is refused while a receiver lists it in its forward-to, and while a job to it is
queued or running unless --cancel-jobs cancels those jobs in the same step. Its
jobs stay, ended, and its name stays held while any job names it, so a name in a
record always means one sender. A repeated command finds no sender and exits 4.

Without --yes nothing is sent: the command says what it would do and exits 2.`,
		Example: `  nopacs sender remove lab1 pacs          # says what would happen, exits 2
  nopacs sender remove lab1 pacs --yes
  nopacs sender remove lab1 pacs --yes --cancel-jobs`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if !yes {
				return fault.New(fault.Invalid, fmt.Sprintf("nothing was removed: this would remove sender %q of tenant %q and its credential; its jobs stay; run it again with --yes to do it", output.Clean(args[1]), output.Clean(args[0])))
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RemoveSenderResponse, error) {
				return c.Senders.RemoveSender(ctx, &v1.RemoveSenderRequest{Tenant: args[0], Name: args[1], CancelJobs: cancelJobs})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "sender %s of tenant %s removed; %d jobs canceled; its jobs stay\n", output.Clean(args[1]), output.Clean(args[0]), res.GetJobsCanceled())
				return err
			})
		}),
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "really remove the sender; without it the command only says what it would do")
	cmd.Flags().BoolVar(&cancelJobs, "cancel-jobs", false, "cancel the sender's jobs that are queued or running, in the same step; without it a sender with such a job is not removed")
	return cmd
}

// newSenderTest builds "sender test".
func (a *app) newSenderTest() *cobra.Command {
	return &cobra.Command{
		Use:   "test <tenant> <name>",
		Short: "Try the sender's peer as a send would reach it, sending no instance",
		Long: `Try a sender's peer the way a send would reach it, and report each step. A dimse
sender: its host resolved and checked against the installation's destinations, a
connection, the TLS handshake when it has TLS, an association that proposes the
Verification SOP Class, a C-ECHO, and the release. A stow sender: the host
resolved and checked, a connection, the TLS handshake for https, and a request
that stores nothing: a search for studies (GET <url>/studies?limit=1, QIDO-RS),
which a DICOMweb service answers 200 or 204. Any other answer fails it: a
redirect (not followed), 404 (the URL does not name a DICOMweb service), 401,
403 or 407 (the credential is not taken), a 5xx (the peer is failing).

The last line says in plain words what was found: that the peer accepted the
association and answered, or the step that failed, why, and the peer's own
status. A peer that fails a step is a result, not an error: the table says
which step failed, and the command exits 1 so a script sees it. A step after a
failed one is skipped, except the release. The test changes nothing, and works
for a stopped sender too.`,
		Example: `  nopacs sender test lab1 pacs`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			// The sender's type says what a C-ECHO status or an HTTP status
			// is, in the sentence that closes the table.
			got, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetSenderResponse, error) {
				return c.Senders.GetSender(ctx, &v1.GetSenderRequest{Tenant: args[0], Name: args[1]})
			})
			if err != nil {
				return err
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.TestSenderResponse, error) {
				return c.Senders.TestSender(ctx, &v1.TestSenderRequest{Tenant: args[0], Name: args[1]})
			})
			if err != nil {
				return err
			}
			if err := messageOut(a, res, func(w io.Writer) error {
				rows := make([][]string, 0, len(res.GetResults()))
				for _, r := range res.GetResults() {
					took := "-"
					if r.GetDuration() != nil {
						took = r.GetDuration().AsDuration().Round(time.Millisecond).String()
					}
					rows = append(rows, []string{output.SenderTestStep(r.GetStep()), output.SenderTestOutcome(r.GetOutcome()), took, output.SenderFault(r.GetFault()), output.Dash(r.GetDetail())})
				}
				if err := output.Table(w, a.style(), []string{"STEP", "OUTCOME", "TOOK", "FAULT", "DETAIL"}, rows); err != nil {
					return err
				}
				_, err := fmt.Fprintln(w, output.Clean(output.SenderTestSummary(res.GetResults(), got.GetSender().GetDimse() != nil)))
				return err
			}); err != nil {
				return err
			}
			if !res.GetPassed() {
				return fault.New(fault.Internal, fmt.Sprintf("sender %s of tenant %s failed its test", output.Clean(args[1]), output.Clean(args[0])))
			}
			return nil
		}),
	}
}
