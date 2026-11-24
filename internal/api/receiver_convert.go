package api

import (
	"context"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/trial"
)

// receiverMask is what SetReceiver's update_mask may name (receiver.proto,
// SetReceiverRequest.update_mask).
var receiverMask = maskRules{
	fields: append([]string{
		string(tenant.FieldEnabled), string(tenant.FieldStorage), string(tenant.FieldNaming), string(tenant.FieldNamespace),
		string(tenant.FieldQuietPeriod), string(tenant.FieldMaxOpenTime), string(tenant.FieldMaxFileBytes),
		string(tenant.FieldDIMSEPort), string(tenant.FieldDIMSEBindAddress), string(tenant.FieldDIMSEAETitle),
		string(tenant.FieldDIMSECallers), string(tenant.FieldDIMSEAnyCaller), string(tenant.FieldDIMSETLS),
		string(tenant.FieldDIMSETLSCert), string(tenant.FieldDIMSETLSKey), string(tenant.FieldDIMSESOPClasses),
		string(tenant.FieldDIMSESyntaxes), string(tenant.FieldSTOWKeys), string(tenant.FieldSTOWMaxRequest),
		string(tenant.FieldForwardTo), string(tenant.FieldDeidentification), string(tenant.FieldRouting),
	}, importMaskFields...),
	refused: map[string]string{
		"dimse":                        "names a type's whole message: name its fields one by one",
		"stow":                         "names a type's whole message: name its fields one by one",
		"importer":                     "names a type's whole message: name its fields one by one",
		"name":                         "is the receiver's name, which does not change",
		"health":                       "is answered by the server and cannot be set",
		"create_time":                  "is answered by the server and cannot be set",
		"update_time":                  "is answered by the server and cannot be set",
		"as_received":                  "is a member of deidentification: name the oneof, \"deidentification\"",
		"profile":                      "is a member of deidentification: name the oneof, \"deidentification\"",
		"routing.routes":               "is part of routing, which is replaced whole: name \"routing\"",
		"routing.quarantine_unmatched": "is part of routing, which is replaced whole: name \"routing\"",
	},
}

// trimAE removes the leading and trailing spaces of an AE title, which are
// not significant (PS3.5 6.2, value representation AE).
func trimAE(ae string) string { return strings.Trim(ae, " ") }

// receiverValues returns the values a request's receiver gives, whatever
// fields are set, for an add or for tenant.ApplyReceiver to take the fields
// a mask names from. The durations and the size are returned as given, with
// whether each was; the defaults are the caller's to fill.
func receiverValues(field string, m *v1.Receiver) (r tenant.Receiver, quiet, open bool, err error) {
	r = tenant.Receiver{Name: m.GetName(), Enabled: m.GetEnabled(), Storage: m.GetStorage(), MaxFileBytes: m.GetMaxFileBytes(), ForwardTo: m.GetForwardTo(),
		Namespace: m.GetNamespace()}
	if len(r.ForwardTo) > tenant.MaxForwardTo {
		return r, false, false, invalid(field+".forward_to", "names at most 16 senders")
	}
	if dm := m.GetDimse(); dm != nil {
		r.Type = tenant.ReceiverDIMSE
		r.DIMSE = tenant.DIMSEReceiver{
			Port: int(dm.GetPort()), BindAddress: dm.GetBindAddress(), AETitle: trimAE(dm.GetAeTitle()),
			SOPClasses: dm.GetSopClasses(), TransferSyntaxes: dm.GetTransferSyntaxes(),
		}
		switch c := dm.GetCallers().(type) {
		case *v1.DimseReceiver_AnyCaller:
			if !c.AnyCaller {
				return r, false, false, invalid(field+".dimse.any_caller", "is true when it is set: a receiver is open to every caller only in so many words")
			}
			r.DIMSE.AnyCaller = true
		case *v1.DimseReceiver_AllowedCallers:
			rules := c.AllowedCallers.GetRules()
			if len(rules) > tenant.MaxCallerRules {
				return r, false, false, invalid(field+".dimse.allowed_callers.rules", "holds at most 256 rules")
			}
			for _, cr := range rules {
				r.DIMSE.Callers = append(r.DIMSE.Callers, tenant.CallerRule{CallingAETitle: trimAE(cr.GetCallingAeTitle()), AddressRange: cr.GetAddressRange()})
			}
		}
		if t := dm.GetTls(); t != nil {
			r.DIMSE.TLS = &tenant.ReceiverTLS{CertificateFile: t.GetCertificateFile(), PrivateKeyFile: t.GetPrivateKeyFile()}
		}
	}
	if sm := m.GetStow(); sm != nil {
		if len(sm.GetKeyIds()) > tenant.MaxSTOWKeys {
			return r, false, false, invalid(field+".stow.key_ids", "holds at most 256 key ids")
		}
		r.Type = tenant.ReceiverSTOW
		r.STOW = tenant.STOWReceiver{Keys: sm.GetKeyIds(), MaxRequestBytes: sm.GetMaxRequestBytes()}
	}
	if im := m.GetImporter(); im != nil {
		r.Type = tenant.ReceiverImport
		r.Import = importReceiverValues(im)
	}
	if pv := m.GetProfile(); pv != nil {
		r.Profile = tenant.ProfileRef{Name: pv.GetName(), Version: pv.GetVersion()}
		if err := r.Profile.Validate(); err != nil {
			return r, false, false, fieldOf(field+".profile", err)
		}
	}
	if rm := m.GetRouting(); rm != nil {
		if r.Routing, err = routingValues(field+".routing", rm); err != nil {
			return r, false, false, err
		}
	}
	if n := m.GetNaming(); n != nil {
		spec := naming.Spec{Subject: n.GetSubject(), Session: n.GetSession(), Scan: n.GetScan()}
		if pt := n.GetPattern(); pt != nil {
			spec.PatternAttribute, spec.Pattern = pt.GetAttribute(), pt.GetExpression()
			if spec.PatternAttribute == "" || spec.Pattern == "" {
				return r, false, false, invalid(field+".naming.pattern", "has its attribute and its expression, both required")
			}
		}
		r.Naming = &spec
	}
	if r.QuietPeriod, quiet, err = wholeSeconds(field+".quiet_period", m.GetQuietPeriod()); err != nil {
		return r, false, false, err
	}
	if r.MaxOpenTime, open, err = wholeSeconds(field+".max_open_time", m.GetMaxOpenTime()); err != nil {
		return r, false, false, err
	}
	return r, quiet, open, nil
}

// receiverFromRequest returns the receiver an AddReceiver request gives,
// with the contract's defaults filled in for what it leaves out, checked by
// every rule that does not depend on what exists.
func receiverFromRequest(field string, m *v1.Receiver) (tenant.Receiver, error) {
	if m == nil {
		return tenant.Receiver{}, invalid(field, "is required")
	}
	if err := itemName(field+".name", m.GetName()); err != nil {
		return tenant.Receiver{}, err
	}
	if m.GetDimse() == nil && m.GetStow() == nil && m.GetImporter() == nil {
		return tenant.Receiver{}, invalid(field, "has a type: dimse, stow or importer")
	}
	if err := checkImportLayoutGiven(field, m); err != nil {
		return tenant.Receiver{}, err
	}
	if err := itemName(field+".storage", m.GetStorage()); err != nil {
		return tenant.Receiver{}, err
	}
	r, quiet, open, err := receiverValues(field, m)
	if err != nil {
		return tenant.Receiver{}, err
	}
	if !quiet {
		r.QuietPeriod = tenant.DefaultQuietPeriod
	}
	if !open {
		r.MaxOpenTime = tenant.DefaultMaxOpenTime
	}
	if r.MaxFileBytes == 0 {
		r.MaxFileBytes = tenant.DefaultMaxFileBytes
	}
	if r.Type == tenant.ReceiverSTOW && r.STOW.MaxRequestBytes == 0 {
		r.STOW.MaxRequestBytes = tenant.DefaultMaxRequestBytes
	}
	if err := r.Validate(); err != nil {
		return tenant.Receiver{}, fieldOf(field, err)
	}
	return r, nil
}

// receiverChange returns the values a SetReceiver request gives, refusing a
// duration or size its mask names with no value: such a field is not reset
// to its default (receiver.proto, SetReceiverRequest.update_mask).
func receiverChange(m *v1.Receiver, fields []tenant.ReceiverField) (tenant.Receiver, error) {
	r, quiet, open, err := receiverValues("receiver", m)
	if err != nil {
		return r, err
	}
	for _, f := range fields {
		switch {
		case f == tenant.FieldQuietPeriod && !quiet:
			return r, invalid("receiver.quiet_period", "is named by update_mask and has no value; a duration is not reset to its default")
		case f == tenant.FieldMaxOpenTime && !open:
			return r, invalid("receiver.max_open_time", "is named by update_mask and has no value; a duration is not reset to its default")
		case f == tenant.FieldMaxFileBytes && r.MaxFileBytes == 0:
			return r, invalid("receiver.max_file_bytes", "is named by update_mask and has no value; a size is not reset to its default")
		case f == tenant.FieldSTOWMaxRequest && r.STOW.MaxRequestBytes == 0:
			return r, invalid("receiver.stow.max_request_bytes", "is named by update_mask and has no value; a size is not reset to its default")
		}
	}
	return r, nil
}

// checkReceiverPlace checks what a receiver may use against what the
// installation allows (place.CheckReceiver). st is the tenant's store,
// whose receiver.ports is read.
func (c *core) checkReceiverPlace(ctx context.Context, st tenant.Store, r tenant.Receiver) error {
	return place.CheckReceiver(ctx, tenantReceiverValues{c: c, st: st}, r)
}

// checkDeidentification checks a receiver's choice of de-identification
// against what exists and what the installation allows: a profile version
// the tenant has (a version is never removed, so one found now stays), or,
// for a receiver that archives as received, the tenant's setting
// receiver.allow-as-received. Each failure is a precondition failure: the
// same request may succeed once the version is made or the setting
// allows it.
func (c *core) checkDeidentification(ctx context.Context, p tenant.Pipeline, st tenant.Store, r tenant.Receiver) error {
	if !r.Deidentifies() {
		allowed, err := c.values().OnTenant(st).AllowAsReceived(ctx)
		if err != nil {
			return err
		}
		if !allowed {
			return fault.New(fault.Precondition, "receiver.as_received: the tenant's setting receiver.allow-as-received does not allow a receiver that archives as received; name a profile version")
		}
		return nil
	}
	return c.checkProfileVersion(ctx, p, "receiver.profile", r.Profile)
}

// checkProfileVersion checks a profile version a receiver or one of its
// routes de-identifies with: one the tenant has, which loads and can
// de-identify a receiver's files (engine.ForReception). Each failure but a
// version that does not load is a precondition failure, its message
// starting with field.
func (c *core) checkProfileVersion(ctx context.Context, p tenant.Pipeline, field string, ref tenant.ProfileRef) error {
	profiles, ok := p.(tenant.Profiles)
	if !ok {
		return errNoProfiles
	}
	v, err := profiles.ProfileVersion(ctx, ref.Name, ref.Version)
	if fault.KindOf(err) == fault.NotFound {
		return fault.Wrap(fault.Precondition, err, field+": the tenant has no profile version "+ref.String())
	}
	if err != nil {
		return err
	}
	loaded, err := profile.Load(v.Name, v.Version, v.Source, v.Expanded, v.Checksum)
	if err != nil {
		return fault.Wrap(fault.Internal, err, field+": the stored profile version "+ref.String()+" does not load")
	}
	if err := engine.ForReception(loaded.Expanded()); err != nil {
		return fault.New(fault.Precondition, field+": the profile version "+ref.String()+" cannot de-identify a receiver's files: "+trial.ReceptionRefusal(err))
	}
	return nil
}

// healthMessage returns a receiver's health as the contract writes it.
func healthMessage(h Health) *v1.ReceiverHealth {
	m := &v1.ReceiverHealth{Detail: h.Detail, Since: timestamp(h.Since)}
	switch h.State {
	case HealthStarting:
		m.State = v1.ReceiverState_RECEIVER_STATE_STARTING
	case HealthUp:
		m.State = v1.ReceiverState_RECEIVER_STATE_UP
	case HealthIdle:
		m.State = v1.ReceiverState_RECEIVER_STATE_IDLE
	case HealthDown:
		m.State = v1.ReceiverState_RECEIVER_STATE_DOWN
		m.DownReason = downReasons[h.Reason]
		if m.DownReason == v1.ReceiverDownReason_RECEIVER_DOWN_REASON_UNSPECIFIED {
			m.DownReason = v1.ReceiverDownReason_RECEIVER_DOWN_REASON_OTHER
		}
	}
	return m
}

// downReasons maps each DownReason to the contract's value of the same name.
var downReasons = map[DownReason]v1.ReceiverDownReason{
	DownDisabled:            v1.ReceiverDownReason_RECEIVER_DOWN_REASON_DISABLED,
	DownAddressInUse:        v1.ReceiverDownReason_RECEIVER_DOWN_REASON_ADDRESS_IN_USE,
	DownPermissionDenied:    v1.ReceiverDownReason_RECEIVER_DOWN_REASON_PERMISSION_DENIED,
	DownAddressNotAvailable: v1.ReceiverDownReason_RECEIVER_DOWN_REASON_ADDRESS_NOT_AVAILABLE,
	DownTLS:                 v1.ReceiverDownReason_RECEIVER_DOWN_REASON_TLS,
	DownNearlyFull:          v1.ReceiverDownReason_RECEIVER_DOWN_REASON_NEARLY_FULL,
	DownTenantNotReady:      v1.ReceiverDownReason_RECEIVER_DOWN_REASON_TENANT_NOT_READY,
	DownStopping:            v1.ReceiverDownReason_RECEIVER_DOWN_REASON_STOPPING,
	DownOther:               v1.ReceiverDownReason_RECEIVER_DOWN_REASON_OTHER,
	DownIncomingLimit:       v1.ReceiverDownReason_RECEIVER_DOWN_REASON_INCOMING_LIMIT,
}

// receiverMessage returns a receiver as the contract writes it, with every
// value stored, defaults included, and its health.
func receiverMessage(r tenant.Receiver, h Health) *v1.Receiver {
	m := &v1.Receiver{
		Name: r.Name, Enabled: r.Enabled, Storage: r.Storage,
		QuietPeriod: seconds(r.QuietPeriod), MaxOpenTime: seconds(r.MaxOpenTime), MaxFileBytes: r.MaxFileBytes,
		Health: healthMessage(h), CreateTime: timestamp(r.Created), UpdateTime: timestamp(r.Updated),
		ForwardTo: r.ForwardTo, Namespace: r.Namespace,
	}
	if r.Naming != nil {
		n := &v1.NamingRule{Subject: r.Naming.Subject, Session: r.Naming.Session, Scan: r.Naming.Scan}
		if r.Naming.Pattern != "" || r.Naming.PatternAttribute != "" {
			n.Pattern = &v1.NamingPattern{Attribute: r.Naming.PatternAttribute, Expression: r.Naming.Pattern}
		}
		m.Naming = n
	}
	if r.Type == tenant.ReceiverDIMSE {
		d := &v1.DimseReceiver{
			Port: int32(r.DIMSE.Port), BindAddress: r.DIMSE.BindAddress, AeTitle: r.DIMSE.AETitle, //nolint:gosec // G115: a port, 1 to 65535.
			SopClasses: r.DIMSE.SOPClasses, TransferSyntaxes: r.DIMSE.TransferSyntaxes,
		}
		if r.DIMSE.AnyCaller {
			d.Callers = &v1.DimseReceiver_AnyCaller{AnyCaller: true}
		} else {
			rules := make([]*v1.CallerRule, 0, len(r.DIMSE.Callers))
			for _, cr := range r.DIMSE.Callers {
				rules = append(rules, &v1.CallerRule{CallingAeTitle: cr.CallingAETitle, AddressRange: cr.AddressRange})
			}
			d.Callers = &v1.DimseReceiver_AllowedCallers{AllowedCallers: &v1.AllowedCallers{Rules: rules}}
		}
		if r.DIMSE.TLS != nil {
			d.Tls = &v1.ReceiverTls{CertificateFile: r.DIMSE.TLS.CertificateFile, PrivateKeyFile: r.DIMSE.TLS.PrivateKeyFile}
		}
		m.Type = &v1.Receiver_Dimse{Dimse: d}
	}
	if r.Type == tenant.ReceiverSTOW {
		m.Type = &v1.Receiver_Stow{Stow: &v1.StowReceiver{KeyIds: r.STOW.Keys, MaxRequestBytes: r.STOW.MaxRequestBytes}}
	}
	if r.Type == tenant.ReceiverImport {
		// The map file is a path: receiverService.message adds it for a
		// caller who may read paths.
		m.Type = &v1.Receiver_Importer{Importer: importReceiverMessage(r.Import, false)}
	}
	m.Routing = routingMessage(r.Routing)
	if r.Deidentifies() {
		m.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: r.Profile.Name, Version: r.Profile.Version}}
	} else {
		m.Deidentification = &v1.Receiver_AsReceived{AsReceived: &v1.AsReceived{}}
	}
	return m
}

// seconds returns a stored duration as the contract writes it, absent for
// zero.
func seconds(d time.Duration) *durationpb.Duration {
	if d == 0 {
		return nil
	}
	return durationpb.New(d)
}

// receiverDetails returns the audit details of a receiver: its type, port,
// AE title and storage, as AddReceiver's entry records them; a stow
// receiver's port is 0 and its AE title empty, having neither.
func receiverDetails(r tenant.Receiver, withStorage bool) map[string]string {
	d := map[string]string{detailType: string(r.Type), detailPort: strconv.Itoa(r.DIMSE.Port), detailAETitle: r.DIMSE.AETitle}
	if withStorage {
		d[detailStorage] = r.Storage
	}
	if r.Type == tenant.ReceiverImport {
		// Its root by name and its layout, never its map file's path.
		d[detailRoot], d[detailLayout] = r.Import.Root, r.Import.Layout.String()
	}
	return d
}

// requestReceiverDetails returns AddReceiver's audit details as the request
// gives them, before it is checked.
func requestReceiverDetails(m *v1.Receiver) map[string]string {
	typ := "none"
	switch {
	case m.GetDimse() != nil:
		typ = string(tenant.ReceiverDIMSE)
	case m.GetStow() != nil:
		typ = string(tenant.ReceiverSTOW)
	case m.GetImporter() != nil:
		typ = string(tenant.ReceiverImport)
	}
	return map[string]string{
		detailType: typ, detailPort: strconv.Itoa(int(m.GetDimse().GetPort())),
		detailAETitle: clip(m.GetDimse().GetAeTitle(), 64), detailStorage: clip(m.GetStorage(), 64),
	}
}
