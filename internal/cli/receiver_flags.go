package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
)

// receiverFlags are the flags that describe a receiver, for add and for set.
type receiverFlags struct {
	typ              string
	allowKeys        []string
	noKeys           bool
	maxRequestSize   string
	storage          string
	port             int
	bind             string
	aet              string
	allowAET         []string
	allowFrom        []string
	allowAETFrom     []string
	anyCaller        bool
	noCallers        bool
	tlsCertificate   string
	tlsPrivateKey    string
	noTLS            bool
	sopClasses       []string
	transferSyntaxes []string
	enabled          bool
	namingSubject    string
	namingSession    string
	namingScan       string
	namingAttribute  string
	namingExpression string
	defaultNaming    bool
	quietPeriod      string
	maxOpenTime      string
	maxFileSize      string
	forwardTo        []string
	profile          string
	asReceived       bool
	// imp are an import receiver's flags (receiver_import.go).
	imp importFlags
}

// add registers the flags on a command. The flags that only make sense when
// changing a receiver (turning TLS off, admitting nobody, returning to the
// default naming) are registered for set only.
func (f *receiverFlags) add(cmd *cobra.Command, forSet bool) {
	fl := cmd.Flags()
	if !forSet {
		fl.StringVar(&f.typ, "type", "dimse", "the receiver's type: dimse, a DICOM listener on a port; stow, the tenant's STOW-RS service for the keys it lists; or import, which takes the files its import jobs read from a directory")
	}
	f.imp.add(cmd)
	fl.StringArrayVar(&f.allowKeys, "allow-key", nil, "stow: take requests made with the key of this id, which must also hold writer or more on the tenant; repeat for more keys")
	fl.StringVar(&f.maxRequestSize, "max-request-size", "", "stow: the largest request it reads, a size such as 16GiB, from 1KiB to 1TiB (default 16GiB)")
	fl.StringVar(&f.storage, "storage", "", "the name of the tenant's storage that what arrives is archived to")
	fl.IntVar(&f.port, "port", 0, "the TCP port to listen on, one of the ports the installation allotted to the tenant; on add, left out, the server gives the lowest of them that is free")
	fl.StringVar(&f.bind, "bind-address", "", "the address of the server to listen on, one of those the setting receiver.bind-addresses lists (default: the installation's default)")
	fl.StringVar(&f.aet, "aet", "", "the receiver's own AE title, the Called AE Title a caller must give: 1 to 16 printable characters")
	fl.BoolVar(&f.enabled, "enabled", false, "whether the receiver accepts data; false, the default, keeps it stopped (--enabled=false stops one)")
	fl.StringArrayVar(&f.allowAET, "allow-aet", nil, "admit the caller that gives this Calling AE Title, from any address; repeat for more callers")
	fl.StringArrayVar(&f.allowFrom, "allow-from", nil, "admit callers connecting from this address range in CIDR form, such as 10.20.0.0/24, with any AE title; repeat for more")
	fl.StringArrayVar(&f.allowAETFrom, "allow-aet-from", nil, "admit the caller that gives this Calling AE Title and connects from this range, written TITLE@CIDR, such as PRISMA1@10.20.0.0/24; repeat for more")
	fl.BoolVar(&f.anyCaller, "any-caller", false, "admit every caller that reaches the port; a receiver is open to everyone only when this says so")
	fl.StringVar(&f.tlsCertificate, "tls-certificate-file", "", "the name of the certificate chain file, PEM, in the tenant's TLS directory on the server")
	fl.StringVar(&f.tlsPrivateKey, "tls-private-key-file", "", "the name of the private key file, PEM, in the tenant's TLS directory on the server; it is a file name, not the key")
	fl.StringArrayVar(&f.sopClasses, "sop-class", nil, "accept this SOP Class UID; repeat for more (default: every storage class the server knows)")
	fl.StringArrayVar(&f.transferSyntaxes, "transfer-syntax", nil, "accept this Transfer Syntax UID; repeat for more (default: every transfer syntax the server knows)")
	fl.StringVar(&f.namingSubject, "naming-subject", "", "the template of a new subject's label, such as sub-{#PatientID}")
	fl.StringVar(&f.namingSession, "naming-session", "", "the template of a new session's label, such as ses-{#StudyInstanceUID}")
	fl.StringVar(&f.namingScan, "naming-scan", "", "the template of a new scan's label, such as scan-{#SeriesInstanceUID}")
	fl.StringVar(&f.namingAttribute, "naming-attribute", "", "the attribute whose value the naming pattern reads, such as PatientComments or (0010,4000)")
	fl.StringVar(&f.namingExpression, "naming-expression", "", "the RE2 pattern with named parts that the templates may use, such as (?P<project>[^^]+)\\^(?P<subject>[^^]+)")
	fl.StringVar(&f.quietPeriod, "quiet-period", "", "how long without a file for a study before its reception is complete: a number and s, m or h, from 5s to 1h (default 60s)")
	fl.StringVar(&f.maxOpenTime, "max-open-time", "", "the longest a reception stays receiving: a number and s, m or h, from 1m to 24h, not less than the quiet period (default 1h)")
	fl.StringArrayVar(&f.forwardTo, "forward-to", nil, "the name of a sender of the tenant to send each reception to when it is archived; repeat for more senders, and --forward-to \"\" on set stops forwarding")
	fl.StringVar(&f.maxFileSize, "max-file-size", "", "the largest file it accepts, a size such as 4GiB, from 1KiB to 256GiB (default 4GiB)")
	fl.StringVar(&f.profile, "profile", "", "de-identify what it receives by this version of one of the tenant's profiles, written NAME@VERSION, such as research@3")
	fl.BoolVar(&f.asReceived, "as-received", false, "archive what it receives as received, without de-identification, where the tenant's setting receiver.allow-as-received allows it")
	cmd.MarkFlagsMutuallyExclusive("profile", "as-received")
	if forSet {
		fl.BoolVar(&f.noKeys, "no-keys", false, "stow: take requests from no key; the receiver stays closed until keys are listed again")
		cmd.MarkFlagsMutuallyExclusive("allow-key", "no-keys")
		fl.BoolVar(&f.noCallers, "no-callers", false, "admit nobody: the receiver stays closed until callers are listed again")
		fl.BoolVar(&f.noTLS, "no-tls", false, "turn TLS off")
		fl.BoolVar(&f.defaultNaming, "default-naming", false, "return to the default naming rule, which carries no value the sender gave")
		cmd.MarkFlagsMutuallyExclusive("tls-certificate-file", "no-tls")
		cmd.MarkFlagsMutuallyExclusive("tls-private-key-file", "no-tls")
		for _, n := range []string{"naming-subject", "naming-session", "naming-scan", "naming-attribute", "naming-expression"} {
			cmd.MarkFlagsMutuallyExclusive("default-naming", n)
		}
	}
	cmd.MarkFlagsRequiredTogether("naming-attribute", "naming-expression")
}

// callers builds the receiver's callers from the flags and the mask path they
// change. It returns nil and no path when no flag spoke of callers, so a new
// receiver is closed.
func (f *receiverFlags) callers(cmd *cobra.Command) (callers any, path string, err error) {
	fl := cmd.Flags()
	listed := fl.Changed("allow-aet") || fl.Changed("allow-from") || fl.Changed("allow-aet-from")
	switch {
	case f.anyCaller && (listed || f.noCallers):
		return nil, "", fault.New(fault.Invalid, "--any-caller admits everyone; it cannot be combined with a list of callers or --no-callers")
	case f.noCallers && listed:
		return nil, "", fault.New(fault.Invalid, "--no-callers admits nobody; it cannot be combined with a list of callers")
	case fl.Changed("any-caller"):
		if !f.anyCaller {
			return nil, "", fault.New(fault.Invalid, "--any-caller=false is not a choice: list callers with --allow-aet, --allow-from or --allow-aet-from, or give --no-callers")
		}
		return &v1.DimseReceiver_AnyCaller{AnyCaller: true}, "dimse.any_caller", nil
	case listed || f.noCallers:
		rules := []*v1.CallerRule{}
		for _, t := range f.allowAET {
			rules = append(rules, &v1.CallerRule{CallingAeTitle: t})
		}
		for _, r := range f.allowFrom {
			rules = append(rules, &v1.CallerRule{AddressRange: r})
		}
		for _, both := range f.allowAETFrom {
			// An AE title may hold "@", a range never does, so the last "@"
			// divides them; each side must be there.
			at := strings.LastIndex(both, "@")
			if at < 0 {
				return nil, "", fault.New(fault.Invalid, fmt.Sprintf("--allow-aet-from %q is not TITLE@CIDR, such as PRISMA1@10.20.0.0/24", output.Clean(both)))
			}
			if at == 0 || at == len(both)-1 {
				missing := "range"
				if at == 0 {
					missing = "title"
				}
				return nil, "", fault.New(fault.Invalid, fmt.Sprintf("--allow-aet-from %q lacks its %s: give both, as TITLE@CIDR, such as PRISMA1@10.20.0.0/24", output.Clean(both), missing))
			}
			rules = append(rules, &v1.CallerRule{CallingAeTitle: both[:at], AddressRange: both[at+1:]})
		}
		return &v1.DimseReceiver_AllowedCallers{AllowedCallers: &v1.AllowedCallers{Rules: rules}}, "dimse.allowed_callers", nil
	}
	return nil, "", nil
}

// nonEmpty drops the empty values of a repeated flag, so --sop-class "" is the
// way to ask for the default set when changing a receiver.
func nonEmpty(values []string) []string {
	out := []string{}
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseDuration reads a length of time a flag takes, a number and s, m or h.
func parseDuration(flag, s string) (*durationpb.Duration, error) {
	d, err := parseSpan(s)
	if err != nil {
		return nil, fault.New(fault.Invalid, fmt.Sprintf("--%s %q is not a length of time; use a number and s, m or h, such as 5m", flag, output.Clean(s)))
	}
	return durationpb.New(d), nil
}

// receiver builds the receiver the flags describe, with the mask paths of
// each field that was given, in the order the flags are read. For add the mask
// is not used; for set it names exactly what the person changed.
func (f *receiverFlags) receiver(cmd *cobra.Command, name string) (*v1.Receiver, []string, error) {
	fl := cmd.Flags()
	r := &v1.Receiver{Name: name}
	dimse := &v1.DimseReceiver{}
	r.Type = &v1.Receiver_Dimse{Dimse: dimse}
	stowFlags := fl.Changed("allow-key") || f.noKeys || fl.Changed("max-request-size")
	isStow, err := f.isStow(cmd, stowFlags)
	if err != nil {
		return nil, nil, err
	}
	stow := &v1.StowReceiver{}
	if isStow {
		r.Type = &v1.Receiver_Stow{Stow: stow}
	}
	var paths []string
	if f.typ == "import" || (fl.Lookup("type") == nil && f.imp.given(cmd)) {
		im, importPaths, err := f.imp.importer(cmd)
		if err != nil {
			return nil, nil, err
		}
		r.Type = &v1.Receiver_Importer{Importer: im}
		paths = append(paths, importPaths...)
	}
	touch := func(flag, path string) bool {
		if fl.Changed(flag) {
			paths = append(paths, path)
			return true
		}
		return false
	}
	if touch("enabled", "enabled") {
		r.Enabled = f.enabled
	}
	if touch("storage", "storage") {
		r.Storage = f.storage
	}
	if touch("allow-key", "stow.key_ids") {
		stow.KeyIds = nonEmpty(f.allowKeys)
	}
	if f.noKeys {
		paths = append(paths, "stow.key_ids")
	}
	if touch("max-request-size", "stow.max_request_bytes") {
		if stow.MaxRequestBytes, err = parseSize("max-request-size", f.maxRequestSize); err != nil {
			return nil, nil, err
		}
	}
	if touch("port", "dimse.port") {
		dimse.Port = int32(f.port) // #nosec G115 -- a port above 2^31 is refused by the server either way
	}
	if touch("bind-address", "dimse.bind_address") {
		dimse.BindAddress = f.bind
	}
	if touch("aet", "dimse.ae_title") {
		dimse.AeTitle = f.aet
	}
	callers, path, err := f.callers(cmd)
	if err != nil {
		return nil, nil, err
	}
	switch c := callers.(type) {
	case *v1.DimseReceiver_AnyCaller:
		dimse.Callers = c
	case *v1.DimseReceiver_AllowedCallers:
		dimse.Callers = c
	}
	if path != "" {
		paths = append(paths, path)
	}
	switch {
	case fl.Changed("tls-certificate-file") != fl.Changed("tls-private-key-file"):
		return nil, nil, fault.New(fault.Invalid, "--tls-certificate-file and --tls-private-key-file go together: give both")
	case fl.Changed("tls-certificate-file"):
		dimse.Tls = &v1.ReceiverTls{CertificateFile: f.tlsCertificate, PrivateKeyFile: f.tlsPrivateKey}
		paths = append(paths, "dimse.tls")
	case f.noTLS:
		paths = append(paths, "dimse.tls")
	}
	if touch("sop-class", "dimse.sop_classes") {
		dimse.SopClasses = nonEmpty(f.sopClasses)
	}
	if touch("transfer-syntax", "dimse.transfer_syntaxes") {
		dimse.TransferSyntaxes = nonEmpty(f.transferSyntaxes)
	}
	named := fl.Changed("naming-subject") || fl.Changed("naming-session") || fl.Changed("naming-scan") || fl.Changed("naming-attribute")
	switch {
	case named:
		if f.namingSubject == "" || f.namingSession == "" || f.namingScan == "" {
			return nil, nil, fault.New(fault.Invalid, "a naming rule has three templates: give --naming-subject, --naming-session and --naming-scan together")
		}
		r.Naming = &v1.NamingRule{Subject: f.namingSubject, Session: f.namingSession, Scan: f.namingScan}
		if f.namingAttribute != "" {
			r.Naming.Pattern = &v1.NamingPattern{Attribute: f.namingAttribute, Expression: f.namingExpression}
		}
		paths = append(paths, "naming")
	case f.defaultNaming:
		paths = append(paths, "naming")
	}
	if touch("quiet-period", "quiet_period") {
		if r.QuietPeriod, err = parseDuration("quiet-period", f.quietPeriod); err != nil {
			return nil, nil, err
		}
	}
	if touch("max-open-time", "max_open_time") {
		if r.MaxOpenTime, err = parseDuration("max-open-time", f.maxOpenTime); err != nil {
			return nil, nil, err
		}
	}
	if touch("forward-to", "forward_to") {
		r.ForwardTo = nonEmpty(f.forwardTo)
	}
	if touch("max-file-size", "max_file_bytes") {
		if r.MaxFileBytes, err = parseSize("max-file-size", f.maxFileSize); err != nil {
			return nil, nil, err
		}
	}
	if err := f.deidentification(cmd, r, &paths); err != nil {
		return nil, nil, err
	}
	return r, paths, nil
}

// dimseFlags are the flags of a DIMSE receiver's own fields.
var dimseFlags = []string{
	"port", "bind-address", "aet", "allow-aet", "allow-from", "allow-aet-from", "any-caller", "no-callers",
	"tls-certificate-file", "tls-private-key-file", "no-tls", "sop-class", "transfer-syntax",
}

// isStow reports whether the flags describe a stow receiver: its --type on
// add; on set, where the type is the stored receiver's, whether a flag of
// the stow type was given. Flags of both types together are refused, since
// a receiver has one.
func (f *receiverFlags) isStow(cmd *cobra.Command, stowFlags bool) (bool, error) {
	fl := cmd.Flags()
	dimse := false
	for _, n := range dimseFlags {
		if fl.Lookup(n) != nil && fl.Changed(n) {
			dimse = true
		}
	}
	stow := stowFlags
	imp := f.imp.given(cmd)
	if fl.Lookup("type") != nil {
		switch f.typ {
		case "dimse":
		case "stow":
			stow = true
		case "import":
			imp = true
		default:
			return false, fault.New(fault.Invalid, fmt.Sprintf("--type %q is not a receiver type; use dimse, stow or import", output.Clean(f.typ)))
		}
		if stowFlags && f.typ != "stow" {
			return false, fault.New(fault.Invalid, "--allow-key and --max-request-size are a stow receiver's: give --type stow")
		}
		if f.imp.given(cmd) && f.typ != "import" {
			return false, fault.New(fault.Invalid, "--root, --layout, --pattern, --unit-depth and the --map-* flags are an import receiver's: give --type import")
		}
	}
	if stow && dimse {
		return false, fault.New(fault.Invalid, "a stow receiver has no port, AE title, callers, TLS, SOP classes or transfer syntaxes: those flags are a dimse receiver's")
	}
	if imp && (dimse || stowFlags) {
		return false, fault.New(fault.Invalid, "an import receiver has no port, AE title, callers, TLS, SOP classes, transfer syntaxes or keys: those flags are a dimse or a stow receiver's")
	}
	return stow, nil
}

// deidentification sets the receiver's choice of de-identification from
// --profile or --as-received, and the mask path of the oneof that holds it.
// A profile is named by an exact version: a receiver runs the version it
// names, and a new version runs nowhere until a receiver is changed to it.
func (f *receiverFlags) deidentification(cmd *cobra.Command, r *v1.Receiver, paths *[]string) error {
	fl := cmd.Flags()
	switch {
	case fl.Changed("profile"):
		ref, err := profile.ParseRef(f.profile)
		if err != nil || ref.Version == 0 {
			return fault.New(fault.Invalid, fmt.Sprintf("--profile %q is not NAME@VERSION, such as research@3: a receiver names the exact version it runs", output.Clean(f.profile)))
		}
		r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: ref.Name, Version: ref.Version}}
	case fl.Changed("as-received"):
		if !f.asReceived {
			return fault.New(fault.Invalid, "--as-received=false is not a choice: name a profile version with --profile")
		}
		r.Deidentification = &v1.Receiver_AsReceived{AsReceived: &v1.AsReceived{}}
	default:
		return nil
	}
	*paths = append(*paths, "deidentification")
	return nil
}
