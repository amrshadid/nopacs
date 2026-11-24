package api

import (
	"net/url"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// MaxCredentialLength is the longest credential a stow sender takes, the
// contract's 4096 bytes before it is sealed.
const MaxCredentialLength = 4096

// senderMask is what SetSender's update_mask may name (sender.proto,
// SetSenderRequest.update_mask).
var senderMask = maskRules{
	fields: []string{
		string(tenant.FieldSenderEnabled), string(tenant.FieldSenderAssociations), string(tenant.FieldSenderByteRate),
		string(tenant.FieldSenderConnectTimeout), string(tenant.FieldSenderResponseTimeout),
		string(tenant.FieldDIMSEHost), string(tenant.FieldDIMSEPeerPort), string(tenant.FieldDIMSECalledAETitle),
		string(tenant.FieldDIMSECallingAETitle), string(tenant.FieldDIMSESenderTLS), string(tenant.FieldDIMSETLSCAFile),
		string(tenant.FieldDIMSETLSCertFile), string(tenant.FieldDIMSETLSKeyFile), string(tenant.FieldDIMSETLSServerName),
		string(tenant.FieldSTOWURL), string(tenant.FieldSTOWAuthentication), string(tenant.FieldSTOWUsername),
		string(tenant.FieldSTOWHeaderName), string(tenant.FieldSTOWCredential), string(tenant.FieldSTOWTLS),
		string(tenant.FieldSTOWTLSCAFile), string(tenant.FieldSTOWTLSCertFile), string(tenant.FieldSTOWTLSKeyFile),
		string(tenant.FieldSTOWTLSServerName), string(tenant.FieldSTOWMaxInstances), string(tenant.FieldSTOWMaxRequestBytes),
	},
	refused: map[string]string{
		"dimse":               "names a type's whole message: name its fields one by one",
		"stow":                "names a type's whole message: name its fields one by one",
		"name":                "is the sender's name, which does not change",
		"health":              "is answered by the server and cannot be set",
		"create_time":         "is answered by the server and cannot be set",
		"update_time":         "is answered by the server and cannot be set",
		"stow.credential_set": "is answered by the server and cannot be set",
	},
}

// senderValues returns the values a request's sender gives, whatever fields
// are set, for an add, or for tenant.ApplySender to take the fields a mask
// names from, with the credential apart, unsealed, and whether each
// duration was given: they are returned as given, the defaults the
// caller's to fill.
//
// A stow sender's authentication is required to be stated when stated is
// set, as an add does; a change states it only when its mask names it, and
// otherwise the field is ignored, as the contract says of every field the
// mask does not name.
func senderValues(field string, m *v1.Sender, stated bool) (s tenant.Sender, credential string, connect, response bool, err error) {
	s = tenant.Sender{
		Name: m.GetName(), Enabled: m.GetEnabled(), MaxAssociations: int(m.GetMaxAssociations()),
		MaxBytesPerSecond: m.GetMaxBytesPerSecond(),
	}
	if dm := m.GetDimse(); dm != nil {
		s.Type = tenant.SenderDIMSE
		s.DIMSE = tenant.DIMSESender{
			Host: dm.GetHost(), Port: int(dm.GetPort()), CalledAETitle: trimAE(dm.GetCalledAeTitle()),
			CallingAETitle: trimAE(dm.GetCallingAeTitle()), TLS: senderTLS(dm.GetTls()),
		}
	}
	if sm := m.GetStow(); sm != nil {
		s.Type = tenant.SenderSTOW
		auth, err := stowAuthentication(field+".stow.authentication", sm.GetAuthentication())
		if err != nil && stated {
			return s, "", false, false, err
		}
		s.STOW = tenant.STOWSender{
			URL: sm.GetUrl(), Authentication: auth, Username: sm.GetUsername(), HeaderName: sm.GetHeaderName(),
			TLS: senderTLS(sm.GetTls()), MaxInstancesPerRequest: int(sm.GetMaxInstancesPerRequest()),
			MaxRequestBytes: sm.GetMaxRequestBytes(),
		}
		credential = sm.GetCredential()
	}
	if s.ConnectTimeout, connect, err = wholeSeconds(field+".connect_timeout", m.GetConnectTimeout()); err != nil {
		return s, "", false, false, err
	}
	if s.ResponseTimeout, response, err = wholeSeconds(field+".response_timeout", m.GetResponseTimeout()); err != nil {
		return s, "", false, false, err
	}
	return s, credential, connect, response, nil
}

// senderTLS returns a sender's TLS settings as the port holds them; nil for
// none.
func senderTLS(t *v1.SenderTls) *tenant.SenderTLS {
	if t == nil {
		return nil
	}
	return &tenant.SenderTLS{
		CAFile: t.GetCaFile(), CertificateFile: t.GetCertificateFile(), PrivateKeyFile: t.GetPrivateKeyFile(),
		ServerName: t.GetServerName(),
	}
}

// stowAuthentication returns the kind of authentication a request names, or
// an Invalid error naming field for none: the contract asks for the kind in
// so many words, never what an absent value means.
func stowAuthentication(field string, a v1.StowAuthentication) (tenant.StowAuthentication, error) {
	name, ok := strings.CutPrefix(a.String(), "STOW_AUTHENTICATION_")
	if !ok || a == v1.StowAuthentication_STOW_AUTHENTICATION_UNSPECIFIED {
		return "", invalid(field, "is required: none, basic, bearer or header, stated in so many words")
	}
	return tenant.StowAuthentication(strings.ToLower(name)), nil
}

// senderCredential returns an Invalid error naming field unless credential
// is one the contract accepts: 1 to 4096 bytes of visible ASCII and spaces,
// not starting or ending with a space. The message never holds it.
func senderCredential(field, credential string) error {
	if credential == "" {
		return invalid(field, "is required and not empty")
	}
	if len(credential) > MaxCredentialLength {
		return invalid(field, "is at most 4096 bytes")
	}
	if credential[0] == ' ' || credential[len(credential)-1] == ' ' {
		return invalid(field, "does not start or end with a space")
	}
	for i := 0; i < len(credential); i++ {
		if credential[i] < 0x20 || credential[i] >= 0x7f {
			return invalid(field, "is visible ASCII and spaces")
		}
	}
	return nil
}

// senderFromRequest returns the sender an AddSender request gives, with the
// contract's defaults filled in and stored, and its credential, unsealed,
// checked by every rule that does not depend on what exists or on what the
// installation allows. The credential is returned apart and never put in a
// message; the sender holds no sealed one yet.
func senderFromRequest(field string, m *v1.Sender) (tenant.Sender, string, error) {
	if m == nil {
		return tenant.Sender{}, "", invalid(field, "is required")
	}
	if err := tenant.ValidateSenderName(m.GetName()); err != nil {
		return tenant.Sender{}, "", fieldOf(field+".name", err)
	}
	if m.GetDimse() == nil && m.GetStow() == nil {
		return tenant.Sender{}, "", invalid(field, "has a type: dimse or stow")
	}
	s, credential, connect, response, err := senderValues(field, m, true)
	if err != nil {
		return tenant.Sender{}, "", err
	}
	if (connect && s.ConnectTimeout == 0) || (response && s.ResponseTimeout == 0) {
		// A zero given in so many words is not "absent", which asks for the
		// default.
		return tenant.Sender{}, "", invalid(field, "has a timeout of zero: leave it out for the default")
	}
	s = s.Normalize()
	check := s
	if s.Type == tenant.SenderSTOW {
		switch {
		case s.STOW.Authentication == tenant.StowAuthNone && credential != "":
			return tenant.Sender{}, "", invalid(field+".stow.credential", "is empty with authentication none")
		case s.STOW.Authentication != tenant.StowAuthNone:
			if err := senderCredential(field+".stow.credential", credential); err != nil {
				return tenant.Sender{}, "", err
			}
			// The port's own rules, with a stand-in for the sealed
			// credential, which is made only once the request is known to
			// be valid.
			check.STOW.SealedCredential = []byte{0}
		}
	}
	// The port first: the store refuses it too, but without the field's
	// name, which a caller needs to know what to correct.
	if err := checkURLPort(field+".stow.url", s); err != nil {
		return tenant.Sender{}, "", err
	}
	if err := check.Validate(); err != nil {
		return tenant.Sender{}, "", fieldOf(field, err)
	}
	return s, credential, nil
}

// checkURLPort returns an Invalid error naming field unless the port a stow
// sender's URL names, when it names one, is 1 to 65535. A port that does
// not fit would otherwise be read as none and checked as the scheme's
// default.
func checkURLPort(field string, s tenant.Sender) error {
	if s.Type != tenant.SenderSTOW {
		return nil
	}
	u, err := url.Parse(s.STOW.URL)
	if err != nil || u.Port() == "" {
		return nil
	}
	if n, err := strconv.Atoi(u.Port()); err != nil || n < 1 || n > 65535 {
		return invalid(field, "has a port that is not 1 to 65535")
	}
	return nil
}

// senderChange returns the values a SetSender request gives, refusing a
// duration, max_associations or size of a stow sender its mask names with
// no value: such a field is not reset to its default (sender.proto,
// SetSenderRequest.update_mask).
func senderChange(m *v1.Sender, fields []tenant.SenderField) (tenant.Sender, string, error) {
	s, credential, connect, response, err := senderValues("sender", m, false)
	if err != nil {
		return s, "", err
	}
	for _, f := range fields {
		var path string
		switch {
		case f == tenant.FieldSTOWAuthentication:
			if _, err := stowAuthentication("sender.stow.authentication", m.GetStow().GetAuthentication()); err != nil {
				return s, "", err
			}
			continue
		case f == tenant.FieldSenderAssociations && s.MaxAssociations == 0:
			path = "max_associations"
		case f == tenant.FieldSenderConnectTimeout && !connect:
			path = "connect_timeout"
		case f == tenant.FieldSenderResponseTimeout && !response:
			path = "response_timeout"
		case f == tenant.FieldSTOWMaxInstances && s.STOW.MaxInstancesPerRequest == 0:
			path = "stow.max_instances_per_request"
		case f == tenant.FieldSTOWMaxRequestBytes && s.STOW.MaxRequestBytes == 0:
			path = "stow.max_request_bytes"
		default:
			continue
		}
		return s, "", invalid("sender."+path, "is named by update_mask and has no value; it is not reset to its default")
	}
	return s, credential, nil
}

// enumOf returns the value of an enum of the contract named by the name the
// port keeps it under, the zero (unspecified) value for a name it does not
// have.
func enumOf[E ~int32](values map[string]int32, prefix, name string) E {
	return E(values[prefix+strings.ToUpper(name)])
}

// senderHealthMessage returns a sender's health as the contract writes it.
func senderHealthMessage(h SenderHealth) *v1.SenderHealth {
	state := h.State
	if state == "" {
		state = tenant.SenderUnknown
	}
	m := &v1.SenderHealth{
		State: enumOf[v1.SenderState](v1.SenderState_value, "SENDER_STATE_", string(state)), Detail: h.Detail,
		Since: timestamp(h.Since), FailedConnections: int32(h.FailedConnections), //nolint:gosec // G115: a count of connections in a row, far below 2^31.
		LastSuccessTime: timestamp(h.LastSuccess),
	}
	if h.Fault != "" {
		m.Fault = enumOf[v1.SenderFault](v1.SenderFault_value, "SENDER_FAULT_", string(h.Fault))
	}
	if state == tenant.SenderDown {
		m.DownReason = enumOf[v1.SenderDownReason](v1.SenderDownReason_value, "SENDER_DOWN_REASON_", string(h.DownReason))
		if m.DownReason == v1.SenderDownReason_SENDER_DOWN_REASON_UNSPECIFIED {
			m.DownReason = v1.SenderDownReason_SENDER_DOWN_REASON_OTHER
		}
		if h.DownReason == tenant.SenderDownPeerFailing {
			m.NextTryTime = timestamp(h.NextTry)
		}
	}
	return m
}

// senderTLSMessage returns a sender's TLS settings as the contract writes
// them; nil for none.
func senderTLSMessage(t *tenant.SenderTLS) *v1.SenderTls {
	if t == nil {
		return nil
	}
	return &v1.SenderTls{CaFile: t.CAFile, CertificateFile: t.CertificateFile, PrivateKeyFile: t.PrivateKeyFile, ServerName: t.ServerName}
}

// senderMessage returns a sender as the contract writes it, with every
// value stored, defaults included, its health, and never its credential:
// only whether one is held.
func senderMessage(s tenant.Sender, h SenderHealth) *v1.Sender {
	m := &v1.Sender{
		Name: s.Name, Enabled: s.Enabled, MaxAssociations: int32(s.MaxAssociations), //nolint:gosec // G115: 1 to 16.
		MaxBytesPerSecond: s.MaxBytesPerSecond, ConnectTimeout: seconds(s.ConnectTimeout),
		ResponseTimeout: seconds(s.ResponseTimeout), Health: senderHealthMessage(h),
		CreateTime: timestamp(s.Created), UpdateTime: timestamp(s.Updated),
	}
	switch s.Type {
	case tenant.SenderDIMSE:
		m.Type = &v1.Sender_Dimse{Dimse: &v1.DimseSender{
			Host: s.DIMSE.Host, Port: int32(s.DIMSE.Port), CalledAeTitle: s.DIMSE.CalledAETitle, //nolint:gosec // G115: a port, 1 to 65535.
			CallingAeTitle: s.DIMSE.CallingAETitle, Tls: senderTLSMessage(s.DIMSE.TLS),
		}}
	case tenant.SenderSTOW:
		auth := enumOf[v1.StowAuthentication](v1.StowAuthentication_value, "STOW_AUTHENTICATION_", string(s.STOW.Authentication))
		m.Type = &v1.Sender_Stow{Stow: &v1.StowSender{
			Url: s.STOW.URL, Authentication: auth, Username: s.STOW.Username, HeaderName: s.STOW.HeaderName,
			CredentialSet: s.STOW.CredentialSet, Tls: senderTLSMessage(s.STOW.TLS),
			MaxInstancesPerRequest: int32(s.STOW.MaxInstancesPerRequest), //nolint:gosec // G115: 1 to 1000.
			MaxRequestBytes:        s.STOW.MaxRequestBytes,
		}}
	}
	return m
}

// testResultMessage returns one step of a test as the contract writes it:
// a skipped step has no duration.
func testResultMessage(r SenderTestResult) *v1.SenderTestResult {
	m := &v1.SenderTestResult{
		Step:    enumOf[v1.SenderTestStep](v1.SenderTestStep_value, "SENDER_TEST_STEP_", string(r.Step)),
		Outcome: enumOf[v1.SenderTestOutcome](v1.SenderTestOutcome_value, "SENDER_TEST_OUTCOME_", string(r.Outcome)),
		Detail:  clip(r.Detail, tenant.MaxFailureLength),
	}
	if r.Outcome != TestSkipped {
		m.Duration = durationpb.New(r.Duration)
	}
	if r.Outcome == TestFailed && r.Fault != "" {
		m.Fault = enumOf[v1.SenderFault](v1.SenderFault_value, "SENDER_FAULT_", string(r.Fault))
	}
	return m
}
