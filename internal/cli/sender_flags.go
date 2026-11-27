package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
)

// maxStowCredential is the contract's limit on a stow credential, 4096 bytes.
// A source is read one byte past it, so a longer one is refused, not cut.
const maxStowCredential = 4096

// senderFlags are the flags that describe a sender, for add and for set.
type senderFlags struct {
	typ             string
	host            string
	port            int
	calledAET       string
	callingAET      string
	url             string
	authentication  string
	username        string
	headerName      string
	credEnv         string
	credFile        string
	credStdin       bool
	maxInstances    int
	maxRequestSize  string
	tlsCAFile       string
	tlsCertificate  string
	tlsPrivateKey   string
	tlsServerName   string
	noTLS           bool
	enabled         bool
	allowIdentified bool
	maxAssociations int
	maxBytesPerSec  string
	connectTimeout  string
	responseTimeout string
}

// dimseSenderFlags and stowSenderFlags are the flags of one type each, so a
// command that mixes them is refused before anything is sent.
var (
	dimseSenderFlags = []string{"host", "port", "called-aet", "calling-aet"}
	stowSenderFlags  = []string{"url", "authentication", "username", "header-name", "credential-env", "credential-file", "credential-stdin", "max-instances-per-request", "max-request-size"}
	senderTLSFlags   = []string{"tls-ca-file", "tls-certificate-file", "tls-private-key-file", "tls-server-name"}
)

// add registers the flags on a command. The type is chosen on add and follows
// from the flags on set, where it cannot change.
func (f *senderFlags) add(cmd *cobra.Command, forSet bool) {
	fl := cmd.Flags()
	if !forSet {
		fl.StringVar(&f.typ, "type", "", "the sender's type: dimse (a DICOM peer reached by C-STORE) or stow (a DICOMweb service reached by STOW-RS) (required)")
		_ = cmd.MarkFlagRequired("type")
		_ = cmd.RegisterFlagCompletionFunc("type", cobra.FixedCompletions([]string{"dimse", "stow"}, cobra.ShellCompDirectiveNoFileComp))
	}
	fl.StringVar(&f.host, "host", "", "dimse: the peer's host name or address")
	fl.IntVar(&f.port, "port", 0, "dimse: the peer's TCP port")
	fl.StringVar(&f.calledAET, "called-aet", "", "dimse: the peer's AE title, the Called AE Title of each association: 1 to 16 printable characters")
	fl.StringVar(&f.callingAET, "calling-aet", "", "dimse: the AE title the server calls with, which the peer commonly admits by: 1 to 16 printable characters")
	fl.StringVar(&f.url, "url", "", "stow: the base URL of the peer's DICOMweb service, such as https://pacs2.lab.example/dicom-web; requests go to <url>/studies")
	fl.StringVar(&f.authentication, "authentication", "", "stow: how requests prove who sends them: "+strings.Join(enumNames(v1.StowAuthentication_name, stowAuthPrefix), ", ")+" (required on add)")
	_ = cmd.RegisterFlagCompletionFunc("authentication", cobra.FixedCompletions(enumNames(v1.StowAuthentication_name, stowAuthPrefix), cobra.ShellCompDirectiveNoFileComp))
	fl.StringVar(&f.username, "username", "", "stow: the user name of basic authentication; it is not a secret")
	fl.StringVar(&f.headerName, "header-name", "", "stow: the name of the header field that carries the credential with --authentication header, such as X-Api-Key")
	fl.StringVar(&f.credEnv, "credential-env", "", "stow: name of the environment variable that holds the credential: the password, the bearer token or the header's value")
	fl.StringVar(&f.credFile, "credential-file", "", "stow: path of a file, readable only by you, that holds the credential")
	fl.BoolVar(&f.credStdin, "credential-stdin", false, "stow: read the credential from standard input, up to its end")
	fl.IntVar(&f.maxInstances, "max-instances-per-request", 0, "stow: the most instances in one request, from 1 to 1000 (default 64)")
	fl.StringVar(&f.maxRequestSize, "max-request-size", "", "stow: the most bytes of files in one request, a size such as 256MiB, from 1MiB to 64GiB (default 256MiB)")
	fl.StringVar(&f.tlsCAFile, "tls-ca-file", "", "the name of the PEM file of certificate authorities the peer's certificate must chain to, in the tenant's TLS directory on the server; required for a dimse sender with TLS")
	fl.StringVar(&f.tlsCertificate, "tls-certificate-file", "", "the name of the PEM certificate chain the server shows the peer, in the tenant's TLS directory; goes with --tls-private-key-file")
	fl.StringVar(&f.tlsPrivateKey, "tls-private-key-file", "", "the name of the PEM private key file of that certificate, in the tenant's TLS directory; it is a file name, not the key")
	fl.StringVar(&f.tlsServerName, "tls-server-name", "", "the name the peer's certificate must be issued for, when it is not the sender's host")
	fl.BoolVar(&f.enabled, "enabled", false, "whether the sender sends; false, the default, keeps it stopped and its jobs wait (--enabled=false stops one)")
	fl.BoolVar(&f.allowIdentified, "allow-identified", false, "whether the sender may also send instances archived as received, without their profile version recorded whole, or with an accepted pixel risk; false, the default, sends only de-identified instances. Needs admin on the installation, never a masked key (--allow-identified=false clears it)")
	fl.IntVar(&f.maxAssociations, "max-associations", 0, "the most associations (for a stow sender, requests) it holds open at once, from 1 to 16 (default 2)")
	fl.StringVar(&f.maxBytesPerSec, "max-bytes-per-second", "", "the most bytes per second it sends, a size such as 10MiB, from 64KiB to 1TiB; 0 for no bound of its own")
	fl.StringVar(&f.connectTimeout, "connect-timeout", "", "how long to wait for a connection, the TLS handshake and an association: a number and s or m, from 1s to 5m (default 30s)")
	fl.StringVar(&f.responseTimeout, "response-timeout", "", "how long to wait for the peer's answer to one C-STORE or request: a number and s, m or h, from 5s to 1h (default 5m)")
	if forSet {
		fl.BoolVar(&f.noTLS, "no-tls", false, "turn TLS off")
		for _, n := range senderTLSFlags {
			cmd.MarkFlagsMutuallyExclusive("no-tls", n)
		}
	}
	cmd.MarkFlagsMutuallyExclusive("credential-env", "credential-file", "credential-stdin")
}

// stowAuthPrefix is what every name of a stow authentication starts with.
const stowAuthPrefix = "STOW_AUTHENTICATION_"

// credentialGiven says whether a flag named a source for the credential.
func (f *senderFlags) credentialGiven(cmd *cobra.Command) bool {
	fl := cmd.Flags()
	return fl.Changed("credential-env") || fl.Changed("credential-file") || fl.Changed("credential-stdin")
}

// credentialFileRefusal returns the message of a credential file refused
// as others may open it, in the words of a credential file: the check is
// the one the key file follows, whose message names a key file.
func credentialFileRefusal(err error) string {
	return strings.ReplaceAll(strings.TrimPrefix(fault.Message(err), "secret: "), "the key file ", "the credential file ")
}

// readCredential reads a stow sender's credential from the environment
// variable, the private file or standard input the flags name, and from
// nowhere else: never an argument, since arguments are visible to every user of
// the machine. The messages name the source and never show any of the
// credential.
func (a *app) readStowCredential(f *senderFlags) (string, error) {
	var v string
	switch {
	case f.credEnv != "":
		v = a.env.Getenv(f.credEnv)
		if strings.TrimSpace(v) == "" {
			return "", fault.New(fault.Invalid, "--credential-env names "+f.credEnv+", which is not set or is empty")
		}
	case f.credFile != "":
		// The same rule as the key file: a secret others can read is a secret
		// others can use.
		file, err := secret.OpenPrivate(f.credFile)
		if err != nil {
			msg := credentialFileRefusal(err)
			if fault.KindOf(err) == fault.NotFound {
				msg = "cannot read --credential-file " + f.credFile + ": it does not exist"
			}
			return "", fault.Wrap(fault.Invalid, err, msg)
		}
		defer func() { _ = file.Close() }()
		b, err := io.ReadAll(io.LimitReader(file, maxStowCredential+1))
		if err != nil {
			return "", fault.Wrap(fault.Invalid, err, "cannot read --credential-file "+f.credFile)
		}
		v = string(b)
		if strings.TrimSpace(v) == "" {
			return "", fault.New(fault.Invalid, "--credential-file "+f.credFile+" is empty")
		}
	case f.credStdin:
		if a.env.Stdin == nil {
			return "", fault.New(fault.Invalid, "--credential-stdin has no standard input to read")
		}
		b, err := io.ReadAll(io.LimitReader(a.env.Stdin, maxStowCredential+1))
		if err != nil {
			return "", fault.Wrap(fault.Invalid, err, "cannot read the credential from standard input")
		}
		v = string(b)
		if strings.TrimSpace(v) == "" {
			return "", fault.New(fault.Invalid, "standard input held no credential")
		}
	default:
		return "", nil
	}
	if len(v) > maxStowCredential+2 {
		return "", fault.New(fault.Invalid, "the credential is longer than 4096 bytes")
	}
	// A line's end is how a person or a tool ends the text; it is not part of
	// the credential.
	v = strings.TrimRight(v, "\r\n")
	if len(v) > maxStowCredential {
		return "", fault.New(fault.Invalid, "the credential is longer than 4096 bytes")
	}
	return v, nil
}

// tls builds the sender's TLS settings from the flags that name them, nil
// when none was given.
func (f *senderFlags) tls(cmd *cobra.Command) *v1.SenderTls {
	fl := cmd.Flags()
	given := false
	for _, n := range senderTLSFlags {
		given = given || fl.Changed(n)
	}
	if !given {
		return nil
	}
	return &v1.SenderTls{CaFile: f.tlsCAFile, CertificateFile: f.tlsCertificate, PrivateKeyFile: f.tlsPrivateKey, ServerName: f.tlsServerName}
}

// changedAny says whether any of the named flags was given.
func changedAny(cmd *cobra.Command, names []string) bool {
	for _, n := range names {
		if cmd.Flags().Lookup(n) != nil && cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

// isStow reports whether the flags describe a stow sender: the --type on add,
// the flags given on set. Flags of both types together are refused, since a
// sender has one.
func (f *senderFlags) isStow(cmd *cobra.Command) (stow, known bool, err error) {
	dimse, st := changedAny(cmd, dimseSenderFlags), changedAny(cmd, stowSenderFlags)
	if dimse && st {
		return false, false, fault.New(fault.Invalid, "a sender is dimse or stow, not both: --host, --port, --called-aet and --calling-aet are a dimse sender's; --url, --authentication, --username, --header-name, the credential flags, --max-instances-per-request and --max-request-size are a stow sender's")
	}
	if cmd.Flags().Lookup("type") != nil {
		switch f.typ {
		case "dimse":
			if st {
				return false, false, fault.New(fault.Invalid, "--type dimse takes --host, --port, --called-aet and --calling-aet, not the stow flags")
			}
			return false, true, nil
		case "stow":
			if dimse {
				return false, false, fault.New(fault.Invalid, "--type stow takes --url and --authentication, not --host, --port, --called-aet or --calling-aet")
			}
			return true, true, nil
		}
		return false, false, fault.New(fault.Invalid, fmt.Sprintf("--type %q is not a sender type; use dimse or stow", output.Clean(f.typ)))
	}
	return st, dimse || st, nil
}

// sender builds the sender the flags describe, the credential apart, and the
// mask path of each field that was given, in the order the flags are read.
// For add the mask is not used; for set it names exactly what the person
// changes. tlsType is the stored sender's type, for the TLS flags of set,
// which are the same for both types; "" when it is not known.
func (f *senderFlags) sender(cmd *cobra.Command, name, tlsType string) (s *v1.Sender, paths []string, err error) {
	fl := cmd.Flags()
	s = &v1.Sender{Name: name}
	stow, known, err := f.isStow(cmd)
	if err != nil {
		return nil, nil, err
	}
	typ := tlsType
	switch {
	case known && stow:
		typ = "stow"
	case known:
		typ = "dimse"
	}
	dimse, st := &v1.DimseSender{}, &v1.StowSender{}
	if typ == "stow" {
		s.Type = &v1.Sender_Stow{Stow: st}
	} else {
		s.Type = &v1.Sender_Dimse{Dimse: dimse}
	}
	touch := func(flag, path string) bool {
		if fl.Changed(flag) {
			paths = append(paths, path)
			return true
		}
		return false
	}
	if touch("enabled", "enabled") {
		s.Enabled = f.enabled
	}
	if touch("allow-identified", "allow_identified") {
		s.AllowIdentified = f.allowIdentified
	}
	if touch("max-associations", "max_associations") {
		s.MaxAssociations = int32(f.maxAssociations) // #nosec G115 -- a value above 2^31 is refused by the server either way
	}
	if touch("max-bytes-per-second", "max_bytes_per_second") {
		if s.MaxBytesPerSecond, err = parseSize("max-bytes-per-second", f.maxBytesPerSec); err != nil {
			return nil, nil, err
		}
	}
	if touch("connect-timeout", "connect_timeout") {
		if s.ConnectTimeout, err = parseDuration("connect-timeout", f.connectTimeout); err != nil {
			return nil, nil, err
		}
	}
	if touch("response-timeout", "response_timeout") {
		if s.ResponseTimeout, err = parseDuration("response-timeout", f.responseTimeout); err != nil {
			return nil, nil, err
		}
	}
	if touch("host", "dimse.host") {
		dimse.Host = f.host
	}
	if touch("port", "dimse.port") {
		dimse.Port = int32(f.port) // #nosec G115 -- a port above 2^31 is refused by the server either way
	}
	if touch("called-aet", "dimse.called_ae_title") {
		dimse.CalledAeTitle = f.calledAET
	}
	if touch("calling-aet", "dimse.calling_ae_title") {
		dimse.CallingAeTitle = f.callingAET
	}
	if touch("url", "stow.url") {
		st.Url = f.url
	}
	if touch("authentication", "stow.authentication") {
		if st.Authentication, err = parseEnum[v1.StowAuthentication]("authentication", f.authentication, v1.StowAuthentication_name, stowAuthPrefix); err != nil {
			return nil, nil, err
		}
	}
	if touch("username", "stow.username") {
		st.Username = f.username
	}
	if touch("header-name", "stow.header_name") {
		st.HeaderName = f.headerName
	}
	if touch("max-instances-per-request", "stow.max_instances_per_request") {
		st.MaxInstancesPerRequest = int32(f.maxInstances) // #nosec G115 -- a value above 2^31 is refused by the server either way
	}
	if touch("max-request-size", "stow.max_request_bytes") {
		if st.MaxRequestBytes, err = parseSize("max-request-size", f.maxRequestSize); err != nil {
			return nil, nil, err
		}
	}
	tls := f.tls(cmd)
	prefix := "dimse"
	if typ == "stow" {
		prefix = "stow"
	}
	if tls != nil {
		if typ == "stow" {
			st.Tls = tls
		} else {
			dimse.Tls = tls
		}
		for _, p := range [][2]string{{"tls-ca-file", "ca_file"}, {"tls-certificate-file", "certificate_file"}, {"tls-private-key-file", "private_key_file"}, {"tls-server-name", "server_name"}} {
			touch(p[0], prefix+".tls."+p[1])
		}
	}
	if f.noTLS {
		paths = append(paths, prefix+".tls")
	}
	return s, paths, nil
}

// setCredential puts the credential in a sender made by sender, which must be
// a stow sender.
func setCredential(s *v1.Sender, credential string) {
	if st := s.GetStow(); st != nil {
		st.Credential = credential
	}
}
