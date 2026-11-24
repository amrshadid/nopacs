package api

import (
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/auditseal"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// auditSealService is AuditSealService: the seal of the installation's log
// from the master, of a tenant's from its own store, checked with the public
// keys derived from the key file (auditseal.PublicKeys), never with keys a
// database holds.
type auditSealService struct{ c *core }

var _ nopacsv1connect.AuditSealServiceHandler = auditSealService{}

// The details of the AuditSealService's audit entries. The range is
// recorded as asked; what a verification found is added to its entry.
const (
	detailFromSeq         = "from_seq"
	detailToSeq           = "to_seq"
	detailExpectedSeq     = "expected_min_seq"
	detailExpectedDigest  = "expected_checkpoint_digest"
	detailIntact          = "intact"
	detailEntriesChecked  = "entries_checked"
	detailCheckpoints     = "checkpoints_checked"
	detailNextSeq         = "next_seq"
	detailEntries         = "entries"
	detailCheckpointsSent = "checkpoints"
)

// MaxProblemDetail bounds AuditVerifyProblem.detail, in bytes
// (audit_seal.proto).
const MaxProblemDetail = 256

// sealRangeDetails returns the details of a request on a range of seq: its
// scope and the range as asked, "last" for a to_seq not given.
func sealRangeDetails(s *v1.Scope, from int64, to *int64) map[string]string {
	last := "last"
	if to != nil {
		last = strconv.FormatInt(*to, 10)
	}
	return map[string]string{detailScope: scopeDetail(s), detailFromSeq: strconv.FormatInt(from, 10), detailToSeq: last}
}

// verifyDetails returns the details of a VerifyAudit's entry before it
// runs: the range, and whether values kept outside were given (the digest
// itself is recorded as given or not, since it is the caller's to keep).
func verifyDetails(r *v1.VerifyAuditRequest) map[string]string {
	d := sealRangeDetails(r.GetScope(), r.GetFromSeq(), r.ToSeq)
	if r.ExpectedMinSeq != nil {
		d[detailExpectedSeq] = strconv.FormatInt(r.GetExpectedMinSeq(), 10)
	}
	if len(r.GetExpectedCheckpointDigest()) > 0 {
		d[detailExpectedDigest] = "given"
	}
	return d
}

// sealRange returns the range a request asks for as auditseal takes it (To
// 0 for the last sealed entry), or an Invalid error naming the field: a
// negative seq, or a to_seq below from_seq (a from_seq of 0 is the first,
// 1).
func sealRange(from int64, to *int64) (int64, int64, error) {
	if from < 0 {
		return 0, 0, invalid("from_seq", "is 0 or more; 0 and 1 are the first entry")
	}
	if to == nil {
		return from, 0, nil
	}
	if *to < 0 {
		return 0, 0, invalid("to_seq", "is not negative")
	}
	if *to < max(from, 1) {
		return 0, 0, invalid("to_seq", "is not below from_seq, nor below 1")
	}
	return from, *to, nil
}

// sealLog is the log a request on a scope reads.
type sealLog struct {
	// reads is the log's store: the master or the tenant's.
	reads master.AuditSealReads
	// name is the log as its checkpoints name it: master.MasterAuditLog or
	// the tenant's id.
	name string
	// tenant is the tenant's name, empty for the installation.
	tenant string
	// st is the tenant's store, nil for the installation: where the
	// request's entries go (core.record).
	st tenant.Store
}

// sealLogOf returns the log of scope for the request d, its store chosen as
// sinkFor chooses the request's log. An error has been recorded.
func (c *core) sealLogOf(ctx context.Context, d access.Decision, scope access.Scope, details map[string]string) (sealLog, error) {
	if scope.IsInstallation() {
		return sealLog{reads: c.master, name: master.MasterAuditLog}, nil
	}
	st, err := c.sinkFor(ctx, d, scope, details)
	if err != nil {
		return sealLog{}, err
	}
	l := sealLog{reads: st, tenant: scope.Tenant, st: st}
	if ref, ok := d.Tenant(); ok {
		l.name = ref.ID
		return l, nil
	}
	// A caller allowed by its role on the installation: the tenant that
	// holds the name now, as tenantStore found it.
	row, err := c.master.TenantByName(ctx, scope.Tenant)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			err = noTenant(d)
		}
		return sealLog{}, c.failWith(d, st, access.Facts{Details: details}, err)
	}
	l.name = row.ID
	return l, nil
}

// auditKeys returns the public keys that check this installation's
// checkpoints, derived from the key file. Only this installation's: no
// tenant is imported into a server of this version from another
// installation, so it holds no other installation's keys to answer.
func (c *core) auditKeys() ([]auditseal.PublicKey, error) {
	if c.installation == "" {
		return nil, fault.New(fault.Internal, "api: the server was built without its installation id")
	}
	return auditseal.PublicKeys(c.keys, c.installation)
}

// otherAnchors returns the anchors the other logs keep of l: the master's
// of a tenant's log, and for the installation's log each tenant's of it. A
// tenant whose database is not open now is left out: its anchor is checked
// when it is, and when the tenant's own log is verified.
func (c *core) otherAnchors(ctx context.Context, l sealLog) ([]master.AuditAnchor, error) {
	if l.st != nil {
		held, err := c.master.AuditAnchors(ctx)
		if err != nil {
			return nil, err
		}
		var out []master.AuditAnchor
		for _, a := range held {
			if a.Log == l.name {
				out = append(out, a)
			}
		}
		return out, nil
	}
	var out []master.AuditAnchor
	page := master.Page{Limit: master.MaxPageLimit}
	for range maxWholeListPages {
		rows, next, err := c.master.ListTenants(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, t := range rows {
			st, err := c.reg.Reader(t.Name)
			if err != nil {
				continue
			}
			held, err := st.AuditAnchors(ctx)
			if err != nil {
				continue
			}
			for _, a := range held {
				if a.Log == master.MasterAuditLog {
					out = append(out, a)
				}
			}
		}
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
	return nil, fault.New(fault.Internal, "api: the list of tenants did not end")
}

// VerifyAudit recomputes a range of a log's chain and checks its
// checkpoints, the anchors the other logs keep of it and the values the
// caller kept outside, with the public keys of the key file and
// audit.seal-interval as it is now. It records the range and what it found.
func (s auditSealService) VerifyAudit(ctx context.Context, req *v1.VerifyAuditRequest) (*v1.VerifyAuditResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.AuditSealServiceVerifyAuditProcedure)
	if err != nil {
		return nil, err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.fail(d, err)
	}
	details := verifyDetails(req)
	l, err := c.sealLogOf(ctx, d, scope, details)
	if err != nil {
		return nil, err
	}
	fail := func(err error) error { return c.failWith(d, l.st, access.Facts{Details: details}, err) }
	o := auditseal.VerifyOptions{Log: l.name, Now: c.now()}
	if o.From, o.To, err = sealRange(req.GetFromSeq(), req.ToSeq); err != nil {
		return nil, fail(err)
	}
	if req.ExpectedMinSeq != nil {
		if req.GetExpectedMinSeq() < 1 {
			return nil, fail(invalid("expected_min_seq", "is 1 or more when given"))
		}
		o.ExpectedMinSeq = req.GetExpectedMinSeq()
	}
	if dg := req.GetExpectedCheckpointDigest(); len(dg) > 0 {
		if len(dg) != master.AuditDigestSize {
			return nil, fail(invalid("expected_checkpoint_digest", "is empty or 32 bytes"))
		}
		o.ExpectedDigest = master.AuditDigest(dg)
	}
	if o.Keys, err = c.auditKeys(); err != nil {
		return nil, fail(err)
	}
	o.Installation = c.installation
	// The wait past which an unsealed entry is a stalled log is two of the
	// intervals the sealing pass runs at now, so the setting is read, not
	// assumed: a failure to read it is the master not answering.
	if o.SealInterval, err = c.values().AuditSealInterval(ctx); err != nil {
		return nil, fail(err)
	}
	if o.Anchors, err = c.otherAnchors(ctx, l); err != nil {
		return nil, fail(err)
	}
	r, err := auditseal.Verify(ctx, l.reads, o)
	if err != nil {
		return nil, fail(err)
	}
	c.record(d, l.st, master.OutcomeDone, "", access.Facts{Details: withDetails(details, map[string]string{
		detailIntact: strconv.FormatBool(r.Intact), detailEntriesChecked: strconv.FormatInt(r.EntriesChecked, 10),
		detailCheckpoints: strconv.FormatInt(r.CheckpointsChecked, 10), detailNextSeq: strconv.FormatInt(r.NextSeq, 10),
		detailProblems: strconv.Itoa(len(r.Problems)),
	})})
	return verifyMessage(r, l.tenant), nil
}

// verifyMessage returns what a verification found as the contract writes
// it, the problems in order of seq.
func verifyMessage(r auditseal.VerifyResult, tenantName string) *v1.VerifyAuditResponse {
	out := &v1.VerifyAuditResponse{Intact: r.Intact, EntriesChecked: r.EntriesChecked, CheckpointsChecked: r.CheckpointsChecked,
		UnsealedEntries: r.Unsealed, NextSeq: r.NextSeq}
	problems := slices.Clone(r.Problems)
	slices.SortStableFunc(problems, func(a, b auditseal.Problem) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	for _, p := range problems[:min(len(problems), auditseal.MaxProblems)] {
		out.Problems = append(out.Problems, &v1.AuditVerifyProblem{Kind: problemKind(p.Kind), Seq: p.Seq,
			Detail: clip(p.Detail, MaxProblemDetail), Time: timestamp(p.Time)})
	}
	if r.Last != nil {
		out.LastCheckpoint = checkpointMessage(*r.Last, tenantName)
	}
	return out
}

// problemKind returns a problem's kind as the contract names it.
func problemKind(k auditseal.ProblemKind) v1.AuditVerifyProblemKind {
	switch k {
	case auditseal.ChainBroken:
		return v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_CHAIN_BROKEN
	case auditseal.EntryMissing:
		return v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_ENTRY_MISSING
	case auditseal.BadSignature:
		return v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_BAD_SIGNATURE
	case auditseal.OutOfLine:
		return v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_CHECKPOINT_OUT_OF_LINE
	case auditseal.AnchorDisagrees:
		return v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_ANCHOR_DISAGREES
	case auditseal.RolledBack:
		return v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_ROLLED_BACK
	case auditseal.SealingStalled:
		return v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_SEALING_STALLED
	}
	return v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_UNSPECIFIED
}

// checkpointMessage returns a checkpoint as the contract writes it, with
// the bytes its signature covers rebuilt from its fields. The digest is the
// one the log keeps, not one computed here, so a digest changed in the
// database shows as one that is not the SHA-256 of the signed text.
func checkpointMessage(cp master.AuditCheckpoint, tenantName string) *v1.AuditCheckpoint {
	m := &v1.AuditCheckpoint{Tenant: tenantName, FromSeq: cp.From, ToSeq: cp.To, Entries: cp.Entries(),
		Head: cp.Head[:], PreviousDigest: cp.Previous[:], Digest: cp.Digest[:], Time: timestamp(cp.Time),
		KeyVersion: int32(min(cp.KeyVersion, master.MaxAuditKeyVersion)), //nolint:gosec // G115: bounded just here.
		Signature:  cp.Signature, InstallationId: cp.Installation, Log: cp.Log, Pruned: cp.Pruned}
	if text, err := auditseal.SignedText(cp); err == nil {
		m.SignedText = text
	}
	return m
}

// The page token of ListAuditCheckpoints: the scope it was made with and
// the seq below which the next page's checkpoints end, so a token is
// refused with another scope.
const checkpointTokenPrefix = "cp1"

// checkpointToken returns the token of the page of checkpoints ending
// below before, for scope.
func checkpointToken(scope string, before int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(checkpointTokenPrefix + "\x00" + scope + "\x00" + strconv.FormatInt(before, 10)))
}

// parseCheckpointToken returns the seq a token names, or an Invalid error
// naming page_token for one this list did not make for scope.
func parseCheckpointToken(token, scope string) (int64, error) {
	bad := invalid("page_token", "is not one this list returned for these arguments")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, bad
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 || parts[0] != checkpointTokenPrefix || parts[1] != scope {
		return 0, bad
	}
	before, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || before < 2 {
		return 0, bad
	}
	return before, nil
}

// ListAuditCheckpoints answers a page of a log's checkpoints, newest first.
func (s auditSealService) ListAuditCheckpoints(ctx context.Context, req *v1.ListAuditCheckpointsRequest) (*v1.ListAuditCheckpointsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.AuditSealServiceListAuditCheckpointsProcedure)
	if err != nil {
		return nil, err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.fail(d, err)
	}
	details := map[string]string{detailScope: scopeDetail(req.GetScope())}
	l, err := c.sealLogOf(ctx, d, scope, details)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, l.st, err)
	}
	var before int64
	if page.Cursor != "" {
		if before, err = parseCheckpointToken(page.Cursor, details[detailScope]); err != nil {
			return nil, c.failIn(d, l.st, err)
		}
	}
	cps, err := l.reads.RecentAuditCheckpoints(ctx, before, page.Limit)
	if err != nil {
		return nil, c.failIn(d, l.st, err)
	}
	c.done(d, l.st)
	out := &v1.ListAuditCheckpointsResponse{Checkpoints: make([]*v1.AuditCheckpoint, 0, len(cps))}
	for _, cp := range cps {
		out.Checkpoints = append(out.Checkpoints, checkpointMessage(cp, l.tenant))
	}
	// The checkpoints of a log follow on from seq 1 without a gap, so the
	// page is the last when it is short or reaches the first.
	if n := len(cps); n == page.Limit && cps[n-1].From > 1 {
		out.NextPageToken = checkpointToken(details[detailScope], cps[n-1].From)
	}
	return out, nil
}

// GetAuditPublicKey answers the public keys that check the installation's
// checkpoints, derived from the key file; no database is read.
func (s auditSealService) GetAuditPublicKey(ctx context.Context, _ *v1.GetAuditPublicKeyRequest) (*v1.GetAuditPublicKeyResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.AuditSealServiceGetAuditPublicKeyProcedure)
	if err != nil {
		return nil, err
	}
	keys, err := c.auditKeys()
	if err != nil {
		return nil, c.fail(d, err)
	}
	out := &v1.GetAuditPublicKeyResponse{InstallationId: c.installation}
	if out.PublicKeys, err = publicKeyMessages(keys); err != nil {
		return nil, c.fail(d, err)
	}
	c.done(d, nil)
	return out, nil
}

// publicKeyMessages returns public keys as the contract writes them, each
// with its PEM form.
func publicKeyMessages(keys []auditseal.PublicKey) ([]*v1.AuditPublicKey, error) {
	out := make([]*v1.AuditPublicKey, 0, len(keys))
	for _, k := range keys {
		pem, err := k.PEM()
		if err != nil {
			return nil, fault.Wrap(fault.Internal, err, "api: a public key cannot be written as PEM")
		}
		out = append(out, &v1.AuditPublicKey{KeyVersion: int32(min(k.Version, master.MaxAuditKeyVersion)), //nolint:gosec // G115: bounded just here.
			PublicKey: k.Key, PublicKeyPem: string(pem), Current: k.Current, InstallationId: k.Installation})
	}
	return out, nil
}
