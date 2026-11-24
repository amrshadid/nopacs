package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The bounds of the forward lookup and of an import (deid.proto).
const (
	// MaxCodeValueBytes bounds a value a caller gives: the longest LO is
	// 64 characters, four bytes each at most in UTF-8; a kilobyte holds
	// any identifier and keeps a request small.
	MaxCodeValueBytes = 1 << 10
	// CodesPerMinute is how many forward lookups one key makes a minute,
	// and how many are made into one tenant: the size of each bucket,
	// which fills again at one a second. Enough for a person working
	// through withdrawals one by one; slow, and loud in the log, for a
	// bulk re-identification, by one key or by many.
	CodesPerMinute = 60
	// ImportsPerHour is how many lookup imports one key makes an hour,
	// stored or refused: an import judges up to tenant.MaxLookupImport
	// values against a table, so its bound keeps it from being a faster
	// forward lookup than Code.
	ImportsPerHour = 10
	// maxCodeReceivers bounds the receivers a forward lookup reads the
	// naming rules of, and maxCodeTables the lookup tables it asks; past
	// either the answer is what was read.
	maxCodeReceivers = 1000
	maxCodeTables    = 1000
)

// The details of a forward lookup's audit entry: counts, never a value.
const (
	detailCodeSubjects = "subjects"
	detailCodeLookups  = "lookups"
)

// The buckets that bound the forward lookup and imports (rateBuckets).
var (
	codeKeyBucket    = bucketKind{name: "code key", size: CodesPerMinute, period: time.Minute}
	codeTenantBucket = bucketKind{name: "code tenant", size: CodesPerMinute, period: time.Minute}
	importKeyBucket  = bucketKind{name: "import key", size: ImportsPerHour, period: time.Hour}
)

// deidService is DeidService: the forward lookup of a value's codes and a
// tenant's lookup tables.
type deidService struct{ c *core }

var _ nopacsv1connect.DeidServiceHandler = deidService{}

// codeAttribute reads the attribute a forward lookup names: a keyword of
// the data dictionary, or a tag of eight hexadecimal digits, of a standard
// element; empty is PatientID. It returns the tag and its VR.
func codeAttribute(s string) (uint32, string, error) {
	if s == "" {
		s = "PatientID"
	}
	var t uint32
	if len(s) == 8 {
		if n, err := strconv.ParseUint(s, 16, 32); err == nil {
			t = uint32(n)
		}
	}
	if t == 0 {
		t = uint32(tag.GlobalDictionary().GetByKeyword(s))
	}
	vr, _, _ := strings.Cut(tag.Tag(t).GetVR(), " ")
	if t == 0 || (t>>16)%2 == 1 || vr == "" || vr == "SQ" || t>>16 <= 0x0007 {
		return 0, "", invalid("attribute", "is a keyword of PS3.6, such as PatientID, or a tag of eight hexadecimal digits, of a standard element with a value")
	}
	return t, vr, nil
}

// codeValue returns a value a caller gives as an element of VR vr holds
// it, its padding removed (PS3.5 6.2), or an Invalid error naming field.
func codeValue(field, vr, s string) (string, error) {
	if len(s) > MaxCodeValueBytes || !utf8.ValidString(s) {
		return "", invalid(field, "is at most 1024 bytes of UTF-8")
	}
	v := naming.TrimValue(vr, s)
	if v == "" {
		return "", invalid(field, "is required, and not only padding")
	}
	if vr == "UI" {
		if err := uid.Check(v); err != nil {
			return "", invalid(field, "is a UID, the attribute's VR being UI")
		}
	}
	return v, nil
}

// issuerOf returns the issuer a request gives of a Patient ID as the
// subject identity holds it (naming.SubjectIdentity: Issuer and
// Qualifiers, in naming.QualifierPaths' order), each value with its
// padding removed for its VR (PS3.5 6.2), as a file's would be; or an
// Invalid error naming the field under prefix. nil gives no issuer.
func issuerOf(prefix string, m *v1.PatientIDIssuer) (naming.SubjectIdentity, error) {
	var id naming.SubjectIdentity
	if m == nil {
		return id, nil
	}
	fac, jur, agy := m.GetAssigningFacility(), m.GetAssigningJurisdiction(), m.GetAssigningAgency()
	// The order is naming.QualifierPaths'; the VRs are PS3.6's.
	fields := []struct {
		name, vr, value string
		into            *string
	}{
		{"issuer", "LO", m.GetIssuer(), &id.Issuer},
		{"universal_entity_id", "UT", m.GetUniversalEntityId(), &id.Qualifiers[0]},
		{"universal_entity_id_type", "CS", m.GetUniversalEntityIdType(), &id.Qualifiers[1]},
		{"identifier_type_code", "CS", m.GetIdentifierTypeCode(), &id.Qualifiers[2]},
		{"assigning_facility.local_namespace_entity_id", "UT", fac.GetLocalNamespaceEntityId(), &id.Qualifiers[3]},
		{"assigning_facility.universal_entity_id", "UT", fac.GetUniversalEntityId(), &id.Qualifiers[4]},
		{"assigning_facility.universal_entity_id_type", "CS", fac.GetUniversalEntityIdType(), &id.Qualifiers[5]},
		{"assigning_jurisdiction.code_value", "SH", jur.GetCodeValue(), &id.Qualifiers[6]},
		{"assigning_jurisdiction.coding_scheme_designator", "SH", jur.GetCodingSchemeDesignator(), &id.Qualifiers[7]},
		{"assigning_jurisdiction.long_code_value", "UC", jur.GetLongCodeValue(), &id.Qualifiers[8]},
		{"assigning_jurisdiction.urn_code_value", "UR", jur.GetUrnCodeValue(), &id.Qualifiers[9]},
		{"assigning_agency.code_value", "SH", agy.GetCodeValue(), &id.Qualifiers[10]},
		{"assigning_agency.coding_scheme_designator", "SH", agy.GetCodingSchemeDesignator(), &id.Qualifiers[11]},
		{"assigning_agency.long_code_value", "UC", agy.GetLongCodeValue(), &id.Qualifiers[12]},
		{"assigning_agency.urn_code_value", "UR", agy.GetUrnCodeValue(), &id.Qualifiers[13]},
	}
	for _, f := range fields {
		if len(f.value) > MaxCodeValueBytes || !utf8.ValidString(f.value) || strings.IndexByte(f.value, 0) >= 0 {
			return id, invalid(prefix+"."+f.name, "is at most 1024 bytes of UTF-8, with no NUL")
		}
		*f.into = naming.TrimValue(f.vr, f.value)
	}
	return id, nil
}

// subjectIdentity returns the subject identity a request gives: the
// Patient ID value, the issuer, and the namespace when there is no issuer,
// checked; fields are the request's names of the issuer and the namespace.
func subjectIdentity(value string, issuer *v1.PatientIDIssuer, namespace, issuerField, namespaceField string) (naming.SubjectIdentity, error) {
	id, err := issuerOf(issuerField, issuer)
	if err != nil {
		return id, err
	}
	if namespace != "" {
		if err := tenant.ValidateName("namespace", namespace); err != nil {
			return id, fieldOf(namespaceField, err)
		}
	}
	if strings.IndexByte(value, 0) >= 0 {
		// A Patient ID is LO text, which holds no NUL; one that did
		// could pose as another identity's encoding.
		return id, invalid("value", "is a Patient ID, which holds no NUL")
	}
	id.PatientID, id.Namespace = value, namespace
	return id, nil
}

// keyOf names the caller whose bucket a forward lookup spends: its key, or
// the local operator.
func keyOf(d access.Decision) string {
	if k, ok := d.Principal().Key(); ok {
		return "key " + k.ID
	}
	return "local " + d.Principal().LocalUser()
}

// Code answers the codes the archive gives one value: its keyed hash, the
// subject label each receiver makes of it and whether the catalog holds
// it, and the code of each lookup table that has one. The value is in no
// log line, audit entry or error.
func (s deidService) Code(ctx context.Context, req *v1.CodeRequest) (*v1.CodeResponse, error) {
	c := s.c
	// The key is checked again first, as for a change: the call writes an
	// audit entry that must be in the log before its answer.
	d, err := c.changing(ctx, nopacsv1connect.DeidServiceCodeProcedure)
	if err != nil {
		return nil, err
	}
	pipe, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	t, vr, err := codeAttribute(req.GetAttribute())
	if err != nil {
		return nil, c.failNow(ctx, d, st, access.Facts{}, err)
	}
	facts := access.Facts{Object: fmt.Sprintf("%08X", t)}
	value, err := codeValue("value", vr, req.GetValue())
	if err != nil {
		return nil, c.failNow(ctx, d, st, facts, err)
	}
	if naming.Tag(t) != naming.TagPatientID && (req.GetIssuer() != nil || req.GetNamespace() != "") {
		return nil, c.failNow(ctx, d, st, facts, invalid("issuer", "and namespace are of a Patient ID: an issuer gives a Patient ID, not another attribute"))
	}
	var id *naming.SubjectIdentity
	if naming.Tag(t) == naming.TagPatientID {
		s, err := subjectIdentity(value, req.GetIssuer(), req.GetNamespace(), "issuer", "namespace")
		if err != nil {
			return nil, c.failNow(ctx, d, st, facts, err)
		}
		id = &s
	}
	if which := c.bounds.take(c.now(), bucket{codeKeyBucket, keyOf(d)}, bucket{codeTenantBucket, req.GetTenant()}); which != "" {
		return nil, c.failNow(ctx, d, st, facts, fault.New(fault.Exhausted, which+" has had "+strconv.Itoa(CodesPerMinute)+
			" forward lookups in the last minute, the most it may; try again in a few seconds"))
	}
	out, err := c.code(ctx, req.GetTenant(), pipe, st, t, value, id)
	if err != nil {
		return nil, c.failNow(ctx, d, st, facts, err)
	}
	facts.Details = map[string]string{detailCodeSubjects: strconv.Itoa(len(out.GetSubjects())), detailCodeLookups: strconv.Itoa(len(out.GetLookups()))}
	entry := d.Entry(master.OutcomeDone, "", facts)
	if err := c.auditNow(ctx, st, entry); err != nil {
		return nil, err
	}
	c.log.Info("api: a forward lookup of a value's codes was answered", "request_id", entry.RequestID, "key_id", entry.KeyID,
		"tenant", req.GetTenant(), "attribute", out.GetAttribute(), "subjects", len(out.GetSubjects()), "lookups", len(out.GetLookups()))
	return out, nil
}

// auditNow writes an audit entry to the tenant's log before the answer,
// as a change's entry is written, never batched or dropped. An entry that
// cannot be written fails the call closed: Unavailable, and the answer is
// not sent, so a forward lookup or an import never answers unaudited.
func (c *core) auditNow(ctx context.Context, st tenant.Store, e master.AuditEntry) error {
	if err := st.AppendAudit(ctx, e); err != nil {
		c.log.Error("api: an audit entry that must be written before the answer could not be; the call was refused",
			"request_id", e.RequestID, "method", e.Method, "error", err.Error())
		return codes.ToConnect(fault.New(fault.Unavailable, "the tenant's audit log cannot be written now; nothing was answered"))
	}
	return nil
}

// failNow is failIn with the entry written before the answer (auditNow):
// the refusal of a forward lookup or an import is evidence too.
func (c *core) failNow(ctx context.Context, d access.Decision, st tenant.Store, facts access.Facts, err error) error {
	outcome, reason := outcomeOf(err)
	if werr := c.auditNow(ctx, st, d.Entry(outcome, reason, facts)); werr != nil {
		return werr
	}
	return c.toAPI(d, err)
}

// code does Code's work once the value is checked. id is the subject
// identity a Patient ID's request gives, nil for another attribute: the
// code and the lookups are then of its key (naming.SubjectIdentity.Key),
// and each receiver's subject label is of the identity under that
// receiver's own namespace.
func (c *core) code(ctx context.Context, tenantName string, pipe tenant.Pipeline, st tenant.Store, t uint32, value string, id *naming.SubjectIdentity) (*v1.CodeResponse, error) {
	h, err := c.reg.NamingHasher(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	key := []byte(value)
	var issuer naming.SubjectIdentity
	if id != nil {
		key, issuer = id.Key(), *id
	}
	out := &v1.CodeResponse{Attribute: fmt.Sprintf("%08X", t), Code: h.Hash(key)}
	catalog, _ := st.(tenant.SubjectReads)
	token := ""
	for read := 0; read < maxCodeReceivers; {
		rs, next, err := pipe.ListReceivers(ctx, master.Page{Limit: min(1000, maxCodeReceivers-read), Cursor: token})
		if err != nil {
			return nil, err
		}
		read += len(rs)
		for _, r := range rs {
			rule, err := r.NamingRule()
			if err != nil {
				continue
			}
			subject, ok, err := rule.HashedSubject(naming.Tag(t), value, issuer, h)
			if !ok || err != nil {
				continue
			}
			sl := &v1.SubjectLabelCode{Receiver: r.Name, SubjectLabel: subject}
			if catalog != nil {
				_, err := catalog.CatalogSubject(ctx, subject)
				switch {
				case err == nil:
					sl.Held = true
				case fault.KindOf(err) != fault.NotFound:
					return nil, err
				}
			}
			out.Subjects = append(out.Subjects, sl)
		}
		if token = next; token == "" {
			break
		}
	}
	p, err := profilesOf(st)
	if err != nil {
		return nil, err
	}
	token = ""
	for read := 0; read < maxCodeTables; {
		tables, next, err := p.ListLookupTables(ctx, master.Page{Limit: min(1000, maxCodeTables-read), Cursor: token})
		if err != nil {
			return nil, err
		}
		read += len(tables)
		for _, tb := range tables {
			code, found, err := p.LookupCode(ctx, tenant.LookupKey{Table: tb.Name, Key: out.GetCode()})
			if err != nil {
				return nil, err
			}
			if found {
				out.Lookups = append(out.Lookups, &v1.LookupTableCode{Table: tb.Name, Code: code})
			}
		}
		if token = next; token == "" {
			break
		}
	}
	return out, nil
}

// ImportLookupCodes stores the codes a person chose in a lookup table, by
// the keyed hash of each value, all or none.
func (s deidService) ImportLookupCodes(ctx context.Context, req *v1.ImportLookupCodesRequest) (*v1.ImportLookupCodesResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.DeidServiceImportLookupCodesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inProfiles(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{tenant.DetailLookupEntries: strconv.Itoa(len(req.GetEntries()))}}
	if err := tenant.ValidateName("lookup table", req.GetTable()); err != nil {
		return nil, c.failNow(ctx, d, st, facts, fieldOf("table", err))
	}
	if n := len(req.GetEntries()); n == 0 || n > tenant.MaxLookupImport {
		return nil, c.failNow(ctx, d, st, facts, invalid("entries", "holds 1 to "+strconv.Itoa(tenant.MaxLookupImport)+" entries"))
	}
	if c.bounds.take(c.now(), bucket{importKeyBucket, keyOf(d)}) != "" {
		return nil, c.failNow(ctx, d, st, facts, fault.New(fault.Exhausted, "this key has made "+strconv.Itoa(ImportsPerHour)+
			" lookup imports in the last hour, the most it may; try again later"))
	}
	h, err := c.reg.NamingHasher(ctx, req.GetTenant())
	if err != nil {
		return nil, c.failNow(ctx, d, st, facts, err)
	}
	entries := make([]tenant.LookupEntry, len(req.GetEntries()))
	for i, e := range req.GetEntries() {
		// Positions are the request's own, from 1, as tenant's
		// validation names them.
		at := "entries: entry " + strconv.Itoa(i+1)
		// An LO value's padding: what a lookup of an identifier meets.
		v, err := codeValue(at+" value", "LO", e.GetValue())
		if err != nil {
			return nil, c.failNow(ctx, d, st, facts, err)
		}
		if strings.IndexByte(v, 0) >= 0 {
			// No text value holds a NUL (PS3.5 6.2); a Patient ID that did
			// could spell another subject identity's encoding, and its
			// entry would answer that subject's files.
			return nil, c.failNow(ctx, d, st, facts, invalid(at+" value", "holds a NUL, which no value of a file holds"))
		}
		if !tenant.ValidLookupCode(e.GetCode()) {
			return nil, c.failNow(ctx, d, st, facts, invalid(at+" code", "is 1 to 16 of A-Z, 0-9 and _"))
		}
		key := []byte(v)
		if e.GetIssuer() != nil || e.GetNamespace() != "" {
			// A Patient ID with its issuer, or its namespace: stored by
			// the subject identity's keyed hash, which a lookup of the
			// Patient ID of a file from that issuer meets.
			id, err := subjectIdentity(v, e.GetIssuer(), e.GetNamespace(), at+" issuer", at+" namespace")
			if err != nil {
				return nil, c.failNow(ctx, d, st, facts, err)
			}
			key = id.Key()
		}
		entries[i] = tenant.LookupEntry{Key: h.Hash(key), Code: e.GetCode()}
	}
	got, err := p.ImportLookupCodes(ctx, req.GetTable(), entries, c.now(), changeEntry(d, access.Facts{}), c.adminGuard(d))
	if err != nil {
		if _, refusal := access.RefusedAtChange(err); refusal != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		var lc *tenant.LookupConflicts
		if errors.As(err, &lc) {
			facts.Details[tenant.DetailLookupConflicts] = strconv.Itoa(lc.Count)
		}
		return nil, c.failNow(ctx, d, st, facts, fieldOf("entries", err))
	}
	return &v1.ImportLookupCodesResponse{Table: req.GetTable(), Added: int32(got.Added), Unchanged: int32(got.Unchanged), Codes: got.Codes}, nil //nolint:gosec // G115: at most tenant.MaxLookupImport.
}

// ListLookupTables answers a page of the tenant's lookup tables with their
// sizes.
func (s deidService) ListLookupTables(ctx context.Context, req *v1.ListLookupTablesRequest) (*v1.ListLookupTablesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.DeidServiceListLookupTablesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inProfiles(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListLookupTables(ctx, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListLookupTablesResponse{Tables: make([]*v1.LookupTable, 0, len(items)), NextPageToken: next}
	for _, t := range items {
		out.Tables = append(out.Tables, &v1.LookupTable{Name: t.Name, Codes: t.Codes})
	}
	return out, nil
}
