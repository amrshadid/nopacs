package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/auditseal"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// testInstallation is the installation id the handlers are given: what
// every checkpoint the tests seal names.
const testInstallation = "inst-test-1"

// sealedStore returns the store of a log: the master's for "", else the
// tenant's.
func (e *env) sealedStore(name string) master.AuditSeals {
	e.t.Helper()
	if name == "" {
		return e.master
	}
	st, err := e.registry.Store(name)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// seal writes the audit entries held, then seals one log ("" for the
// installation's) as the server's pass does, signed with the key derived
// from the test's key file, and returns its latest checkpoint.
func (e *env) seal(name string) master.AuditCheckpoint {
	e.t.Helper()
	e.flush()
	log := master.MasterAuditLog
	if name != "" {
		row, err := e.master.TenantByName(context.Background(), name)
		if err != nil {
			e.t.Fatal(err)
		}
		log = row.ID
	}
	d, err := e.keys.Derive(secret.AuditSeal)
	if err != nil {
		e.t.Fatal(err)
	}
	signer, err := d.Signer()
	if err != nil {
		e.t.Fatal(err)
	}
	last, ok, err := auditseal.SealLog(context.Background(), e.sealedStore(name), testInstallation, log, signer, e.now, nil)
	if err != nil || !ok {
		e.t.Fatalf("sealing log %q: %v (sealed %v)", name, err, ok)
	}
	return last
}

// sealAgain adds one entry to a log and seals it, so the log gains one more
// checkpoint.
func (e *env) sealAgain(name string) master.AuditCheckpoint {
	e.t.Helper()
	if err := e.sealedStore(name).(interface {
		AppendAudit(context.Context, ...master.AuditEntry) error
	}).AppendAudit(context.Background(), e.setupEntry()); err != nil {
		e.t.Fatal(err)
	}
	return e.seal(name)
}

// verify returns a call of VerifyAudit.
func verify(r *v1.VerifyAuditRequest) func(clients) outcome {
	return func(c clients) outcome { return call(c.auditSeal.VerifyAudit, r) }
}

// verified returns a check that the verification answered intact as want,
// with the problem kinds given in order.
func verified(intact bool, kinds ...v1.AuditVerifyProblemKind) func(t *testing.T, o outcome) {
	return func(t *testing.T, o outcome) {
		t.Helper()
		r := o.res.(*v1.VerifyAuditResponse)
		if r.GetIntact() != intact || len(r.GetProblems()) != len(kinds) {
			t.Fatalf("intact %v with problems %v, want %v with %v", r.GetIntact(), r.GetProblems(), intact, kinds)
		}
		for i, k := range kinds {
			if r.GetProblems()[i].GetKind() != k {
				t.Errorf("problem %d is %v, want %v", i, r.GetProblems()[i].GetKind(), k)
			}
		}
	}
}

// seq is a pointer to n, for an optional seq.
func seq(n int64) *int64 { return &n }

// testVerifyAudit guards VerifyAudit: admin on the scope asked for; the
// installation's log and a tenant's verified intact with the keys of the key
// file; the range's bounds and every invalid field the contract lists;
// values kept outside the installation (a minimum seq, a checkpoint's
// digest) that the log does not reach, answered rolled back; a log whose
// unsealed entries waited past two of audit.seal-interval's intervals,
// answered stalled with the time of the oldest, the interval read from the
// setting; the verification recorded in the log of its scope, with its
// range and whether it was intact.
func testVerifyAudit(t *testing.T) {
	f := settingsFixture(t)
	last := f.seal("")
	f.sealAgain("lab1")
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: verify(&v1.VerifyAuditRequest{Scope: installation()}), code: connect.CodeUnauthenticated},
		{name: "installation, just below the role", as: f.instOperator, call: verify(&v1.VerifyAuditRequest{Scope: installation()}), code: connect.CodePermissionDenied},
		{name: "installation, at the role", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation()}),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"scope": "installation", "from_seq": "0", "to_seq": "last", "intact": "true", "next_seq": "0"}},
			check: func(t *testing.T, o outcome) {
				verified(true)(t, o)
				r := o.res.(*v1.VerifyAuditResponse)
				lc := r.GetLastCheckpoint()
				if r.GetEntriesChecked() != last.To || r.GetCheckpointsChecked() < 1 || lc.GetToSeq() != last.To || lc.GetLog() != "master" ||
					lc.GetInstallationId() != testInstallation || !bytes.Equal(lc.GetDigest(), last.Digest[:]) || lc.GetTenant() != "" {
					t.Errorf("answer %v, the log sealed to %d", r, last.To)
				}
			}},
		{name: "the first seq alone", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), FromSeq: 1, ToSeq: seq(1)}), check: verified(true)},
		{name: "a negative from_seq", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), FromSeq: -1}),
			code: connect.CodeInvalidArgument, msg: "from_seq", audit: failed("from_seq")},
		{name: "a negative to_seq", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), ToSeq: seq(-1)}), code: connect.CodeInvalidArgument, msg: "to_seq"},
		{name: "a to_seq below from_seq", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), FromSeq: 5, ToSeq: seq(4)}), code: connect.CodeInvalidArgument, msg: "to_seq"},
		{name: "a to_seq of 0", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), ToSeq: seq(0)}), code: connect.CodeInvalidArgument, msg: "to_seq"},
		{name: "an expected_min_seq below 1", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), ExpectedMinSeq: seq(0)}),
			code: connect.CodeInvalidArgument, msg: "expected_min_seq"},
		{name: "a digest of 31 bytes", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), ExpectedCheckpointDigest: make([]byte, 31)}),
			code: connect.CodeInvalidArgument, msg: "expected_checkpoint_digest"},
		{name: "a seq the log reached", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), ExpectedMinSeq: seq(last.To)}), check: verified(true)},
		{name: "a seq the log does not reach", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), ExpectedMinSeq: seq(last.To + 100)}),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"intact": "false", "expected_min_seq": "*"}},
			check: verified(false, v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_ROLLED_BACK)},
		{name: "a digest the log holds", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), ExpectedCheckpointDigest: last.Digest[:]}),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"expected_checkpoint_digest": "given"}}, check: verified(true)},
		{name: "a digest the log does not hold", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation(), ExpectedCheckpointDigest: bytes.Repeat([]byte{7}, 32)}),
			check: verified(false, v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_ROLLED_BACK)},
		{name: "no scope", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{}), code: connect.CodeInvalidArgument, msg: "scope"},
		{name: "tenant, an installation admin", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: tenantScope("lab1")}), code: connect.CodePermissionDenied},
		{name: "tenant, just below the role", as: f.lab1Operator, call: verify(&v1.VerifyAuditRequest{Scope: tenantScope("lab1")}), code: connect.CodePermissionDenied},
		{name: "tenant, at the role", as: f.lab1Admin, call: verify(&v1.VerifyAuditRequest{Scope: tenantScope("lab1")}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", details: map[string]string{"scope": "tenant/lab1", "intact": "true"}},
			check: func(t *testing.T, o outcome) {
				verified(true)(t, o)
				if lc := o.res.(*v1.VerifyAuditResponse).GetLastCheckpoint(); lc.GetTenant() != "lab1" || lc.GetLog() == "master" || lc.GetLog() == "" {
					t.Errorf("the tenant's last checkpoint names %q, log %q", lc.GetTenant(), lc.GetLog())
				}
			}},
		{name: "tenant, a negative from_seq", as: f.lab1Admin, call: verify(&v1.VerifyAuditRequest{Scope: tenantScope("lab1"), FromSeq: -2}),
			code: connect.CodeInvalidArgument, msg: "from_seq", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "no such tenant, may list", as: local, call: verify(&v1.VerifyAuditRequest{Scope: tenantScope("nolab")}), code: connect.CodeNotFound},
		{name: "no such tenant, no right to know", as: f.lab1Admin, call: verify(&v1.VerifyAuditRequest{Scope: tenantScope("nolab")}), code: connect.CodePermissionDenied},
		{name: "a tenant that is not ready", as: local, call: verify(&v1.VerifyAuditRequest{Scope: tenantScope("down")}), code: connect.CodeUnavailable, audit: inMasterRerouted("down")},
	})

	// A stalled log: the entries above wait unsealed. With audit.seal-interval
	// at an hour, 25 minutes is not a stall; at its default of ten, it is.
	call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: "audit.seal-interval", Scope: installation(), Value: "1h"}})
	f.flush()
	f.advance(25 * time.Minute)
	f.run(t, []tcase{{name: "waiting less than two intervals of the setting", as: f.instAdmin,
		call: verify(&v1.VerifyAuditRequest{Scope: installation()}), check: verified(true)}})
	call(f.as(local).settings.UnsetSetting, &v1.UnsetSettingRequest{Name: "audit.seal-interval", Scope: installation()})
	f.flush()
	f.run(t, []tcase{{name: "waiting more than two intervals", as: f.instAdmin, call: verify(&v1.VerifyAuditRequest{Scope: installation()}),
		check: func(t *testing.T, o outcome) {
			verified(false, v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_SEALING_STALLED)(t, o)
			p := o.res.(*v1.VerifyAuditResponse).GetProblems()[0]
			if p.GetSeq() != 0 || p.GetTime() == nil || !p.GetTime().AsTime().After(t0) || p.GetTime().AsTime().After(t0.Add(10*time.Minute)) || p.GetDetail() == "" {
				t.Errorf("the stall is %v; want seq 0 and the oldest unsealed entry's time, before the clock moved 25 minutes", p)
			}
			if o.res.(*v1.VerifyAuditResponse).GetUnsealedEntries() == 0 {
				t.Error("no unsealed entry counted")
			}
		}}})
}

// listCheckpoints returns a call of ListAuditCheckpoints.
func listCheckpoints(r *v1.ListAuditCheckpointsRequest) func(clients) outcome {
	return func(c clients) outcome { return call(c.auditSeal.ListAuditCheckpoints, r) }
}

// testListAuditCheckpoints guards ListAuditCheckpoints: admin on the scope
// asked for; newest first; paged by page_size, the token refused with
// another scope or when it is not one the list made; the page bounds; each
// checkpoint with the bytes its signature covers, which the public key
// checks; a tenant's in its own log.
func testListAuditCheckpoints(t *testing.T) {
	f := settingsFixture(t)
	f.seal("")
	f.sealAgain("")
	newest := f.sealAgain("")
	f.sealAgain("lab1")
	pub := f.publicKey(t)
	var token string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: installation()}), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: installation()}), code: connect.CodePermissionDenied},
		{name: "a page of two", as: f.instAdmin, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: installation(), PageSize: 2}),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"scope": "installation"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ListAuditCheckpointsResponse)
				token = r.GetNextPageToken()
				cs := r.GetCheckpoints()
				if len(cs) != 2 || token == "" || cs[0].GetToSeq() != newest.To || cs[1].GetToSeq() != cs[0].GetFromSeq()-1 {
					t.Fatalf("page %v, token %q; newest to %d", cs, token, newest.To)
				}
				for _, c := range cs {
					if !ed25519.Verify(pub, c.GetSignedText(), c.GetSignature()) || c.GetEntries() != c.GetToSeq()-c.GetFromSeq()+1 ||
						c.GetKeyVersion() != 1 || len(c.GetHead()) != 32 || len(c.GetPreviousDigest()) != 32 || c.GetTime() == nil {
						t.Errorf("checkpoint %v does not check with the public key", c)
					}
				}
			}},
		{name: "the last page", as: f.instAdmin, call: func(c clients) outcome {
			return listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: installation(), PageSize: 2, PageToken: token})(c)
		}, check: func(t *testing.T, o outcome) {
			r := o.res.(*v1.ListAuditCheckpointsResponse)
			if cs := r.GetCheckpoints(); len(cs) != 1 || cs[0].GetFromSeq() != 1 || r.GetNextPageToken() != "" {
				t.Errorf("last page %v, token %q", cs, r.GetNextPageToken())
			}
		}},
		{name: "the token with another scope", as: local, call: func(c clients) outcome {
			return listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: tenantScope("lab1"), PageToken: token})(c)
		}, code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "a token the list did not make", as: f.instAdmin, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: installation(), PageToken: base64.RawURLEncoding.EncodeToString([]byte("cp1\x00installation\x00x"))}),
			code: connect.CodeInvalidArgument, msg: "page_token", audit: failed("page_token")},
		{name: "page_size at 1000", as: f.instAdmin, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: installation(), PageSize: 1000})},
		{name: "page_size at 1001", as: f.instAdmin, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: installation(), PageSize: 1001}), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "a negative page_size", as: f.instAdmin, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: installation(), PageSize: -1}), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "page_token at 1025 bytes", as: f.instAdmin, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: installation(), PageToken: strings.Repeat("t", 1025)}), code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "no scope", as: f.instAdmin, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{}), code: connect.CodeInvalidArgument, msg: "scope"},
		{name: "tenant, at the role", as: f.lab1Admin, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: tenantScope("lab1")}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1"},
			check: func(t *testing.T, o outcome) {
				if cs := o.res.(*v1.ListAuditCheckpointsResponse).GetCheckpoints(); len(cs) != 1 || cs[0].GetTenant() != "lab1" {
					t.Errorf("the tenant's checkpoints: %v", cs)
				}
			}},
		{name: "tenant, just below the role", as: f.lab1Operator, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: tenantScope("lab1")}), code: connect.CodePermissionDenied},
		{name: "no such tenant, may list", as: local, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: tenantScope("nolab")}), code: connect.CodeNotFound},
		{name: "a tenant that is not ready", as: local, call: listCheckpoints(&v1.ListAuditCheckpointsRequest{Scope: tenantScope("down")}), code: connect.CodeUnavailable, audit: inMasterRerouted("down")},
	})
}

// publicKey returns the installation's current audit public key as
// GetAuditPublicKey answers it to the local operator.
func (f *fixture) publicKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	o := call(f.as(local).auditSeal.GetAuditPublicKey, &v1.GetAuditPublicKeyRequest{})
	if o.err != nil {
		t.Fatal(o.err)
	}
	ks := o.res.(*v1.GetAuditPublicKeyResponse).GetPublicKeys()
	if len(ks) != 1 {
		t.Fatalf("keys %v", ks)
	}
	return ed25519.PublicKey(ks[0].GetPublicKey())
}

// testGetAuditPublicKey guards GetAuditPublicKey: any authenticated caller,
// even one with no grant; the installation's id and one key per version of
// the key file, the current one marked, its PEM the same key; derived from
// the key file, so the same key as the sealing signer's; one entry in the
// installation's log.
func testGetAuditPublicKey(t *testing.T) {
	f := newFixture(t)
	d, err := f.keys.Derive(secret.AuditSeal)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := d.Signer()
	if err != nil {
		t.Fatal(err)
	}
	get := func(c clients) outcome { return call(c.auditSeal.GetAuditPublicKey, &v1.GetAuditPublicKeyRequest{}) }
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: get, code: connect.CodeUnauthenticated},
		{name: "a key with no grant", as: f.noGrant, call: get, audit: &wantAudit{outcome: master.OutcomeDone},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.GetAuditPublicKeyResponse)
				ks := r.GetPublicKeys()
				if r.GetInstallationId() != testInstallation || len(ks) != 1 {
					t.Fatalf("answer %v", r)
				}
				k := ks[0]
				if k.GetKeyVersion() != 1 || !k.GetCurrent() || k.GetInstallationId() != testInstallation || !bytes.Equal(k.GetPublicKey(), signer.Public()) ||
					!strings.HasPrefix(k.GetPublicKeyPem(), "-----BEGIN PUBLIC KEY-----\n") {
					t.Errorf("key %v; the signer's public key is %x", k, []byte(signer.Public()))
				}
			}},
		{name: "the local operator", as: local, call: get, audit: done()},
	})
}

// exportOnce reads a whole export and returns its first message or its
// error as an outcome: what the tables of every method need of it.
func exportOnce(c clients, r *v1.ExportAuditRequest) outcome {
	res := exportAll(c, r)
	o := outcome{err: res.err, requestID: res.requestID}
	if res.err == nil && len(res.msgs) > 0 {
		o.res = res.msgs[0]
	}
	return o
}

// exportResult is what one ExportAudit stream carried.
type exportResult struct {
	msgs      []*v1.ExportAuditResponse
	err       error
	requestID string
}

// exportAll reads a whole export.
func exportAll(c clients, r *v1.ExportAuditRequest) exportResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx, info := connect.NewClientContext(ctx)
	var out exportResult
	st, err := c.auditSeal.ExportAudit(ctx, r)
	if err == nil {
		defer func() { _ = st.Close() }()
		for {
			m, rerr := st.Receive()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				err = rerr
				break
			}
			out.msgs = append(out.msgs, m)
		}
	}
	out.err, out.requestID = err, info.ResponseHeader().Get(api.RequestIDHeader)
	return out
}

// testExportAudit guards ExportAudit: admin on the scope asked for; the
// header first (the installation, the log, the keys of the key file, the
// chain value before the first entry, the anchors), then each entry in
// order of seq with its encoded form and chain value, each checkpoint after
// the last entry it covers; a range started inside a checkpoint moved back
// to its start, with the chain value of the checkpoint before; a to_seq
// that ends at the checkpoint covering it; the invalid ranges; two entries
// in the log of its scope, when it opens and when it ends with its count.
func testExportAudit(t *testing.T) {
	f := settingsFixture(t)
	first := f.seal("")
	second := f.sealAgain("")
	third := f.sealAgain("")
	tenantLast := f.sealAgain("lab1")
	if err := f.master.SetAuditAnchor(ctx, master.AuditAnchor{Log: tenantLast.Log, To: tenantLast.To, Digest: tenantLast.Digest}); err != nil {
		t.Fatal(err)
	}
	pub := f.publicKey(t)

	whole := exportAll(f.as(f.instAdmin), &v1.ExportAuditRequest{Scope: installation()})
	if whole.err != nil {
		t.Fatal(whole.err)
	}
	h := whole.msgs[0].GetHeader()
	if h == nil || h.GetInstallationId() != testInstallation || h.GetLog() != "master" || h.GetFromSeq() != 1 || h.GetEncodingVersion() != 1 ||
		!bytes.Equal(h.GetChainBefore(), make([]byte, 32)) || len(h.GetPublicKeys()) != 1 || !bytes.Equal(h.GetPublicKeys()[0].GetPublicKey(), pub) {
		t.Fatalf("header %v", h)
	}
	if as := h.GetAnchors(); len(as) != 1 || as[0].GetLog() != tenantLast.Log || as[0].GetToSeq() != tenantLast.To || !bytes.Equal(as[0].GetDigest(), tenantLast.Digest[:]) {
		t.Errorf("the header's anchors %v; the master keeps lab1's at %d", as, tenantLast.To)
	}
	next, checkpoints := int64(1), 0
	for _, m := range whole.msgs[1:] {
		switch {
		case m.GetEntry() != nil:
			e := m.GetEntry()
			if e.GetSeq() != next || len(e.GetEncoded()) == 0 || len(e.GetChain()) != 32 || e.GetEntry().GetId() == "" {
				t.Fatalf("entry %v, want seq %d", e, next)
			}
			next++
		case m.GetCheckpoint() != nil:
			c := m.GetCheckpoint()
			if c.GetToSeq() != next-1 || !ed25519.Verify(pub, c.GetSignedText(), c.GetSignature()) {
				t.Fatalf("checkpoint %v after seq %d", c, next-1)
			}
			checkpoints++
		default:
			t.Fatalf("a second header or an empty message: %v", m)
		}
	}
	if next-1 != third.To || checkpoints != 3 {
		t.Errorf("the export carried seq up to %d in %d checkpoints; the log is sealed to %d in 3", next-1, checkpoints, third.To)
	}
	var log []master.AuditEntry
	for _, e := range f.masterLog() {
		if e.RequestID == whole.requestID {
			log = append(log, e)
		}
	}
	if len(log) != 2 {
		t.Fatalf("%d entries of the export, want 2: %v", len(log), log)
	}
	opened, ended := log[1], log[0]
	if opened.Details["stream"] != "opened" || opened.Details["from_seq"] != "0" || opened.Details["to_seq"] != "last" ||
		ended.Details["stream"] != "ended" || ended.Details["entries"] != strconv.FormatInt(third.To, 10) || ended.Details["checkpoints"] != "3" ||
		ended.Details["ended"] != "complete" || ended.Outcome != master.OutcomeDone {
		t.Errorf("the export's entries: %v / %v", opened, ended)
	}

	// A range inside the second checkpoint starts at its start, from the
	// first checkpoint's head; a to_seq inside it ends with it.
	part := exportAll(f.as(f.instAdmin), &v1.ExportAuditRequest{Scope: installation(), FromSeq: second.To, ToSeq: seq(second.To)})
	if part.err != nil {
		t.Fatal(part.err)
	}
	ph := part.msgs[0].GetHeader()
	if ph.GetFromSeq() != second.From || !bytes.Equal(ph.GetChainBefore(), first.Head[:]) {
		t.Errorf("a range in the second checkpoint: header from %d chain %x; want %d and the first's head", ph.GetFromSeq(), ph.GetChainBefore(), second.From)
	}
	lastMsg := part.msgs[len(part.msgs)-1].GetCheckpoint()
	if lastMsg == nil || lastMsg.GetToSeq() != second.To || int64(len(part.msgs)) != 1+second.Entries()+1 {
		t.Errorf("a range in the second checkpoint carried %d messages ending %v", len(part.msgs), lastMsg)
	}

	for _, c := range []struct {
		name  string
		as    caller
		req   *v1.ExportAuditRequest
		code  connect.Code
		field string
	}{
		{"no key", nobody, &v1.ExportAuditRequest{Scope: installation()}, connect.CodeUnauthenticated, ""},
		{"just below the role", f.instOperator, &v1.ExportAuditRequest{Scope: installation()}, connect.CodePermissionDenied, ""},
		{"a negative from_seq", f.instAdmin, &v1.ExportAuditRequest{Scope: installation(), FromSeq: -1}, connect.CodeInvalidArgument, "from_seq"},
		{"a negative to_seq", f.instAdmin, &v1.ExportAuditRequest{Scope: installation(), ToSeq: seq(-1)}, connect.CodeInvalidArgument, "to_seq"},
		{"a to_seq below from_seq", f.instAdmin, &v1.ExportAuditRequest{Scope: installation(), FromSeq: 3, ToSeq: seq(2)}, connect.CodeInvalidArgument, "to_seq"},
		{"no scope", f.instAdmin, &v1.ExportAuditRequest{}, connect.CodeInvalidArgument, "scope"},
		{"tenant, an installation admin", f.instAdmin, &v1.ExportAuditRequest{Scope: tenantScope("lab1")}, connect.CodePermissionDenied, ""},
		{"no such tenant, may list", local, &v1.ExportAuditRequest{Scope: tenantScope("nolab")}, connect.CodeNotFound, ""},
		{"no such tenant, no right to know", f.lab1Admin, &v1.ExportAuditRequest{Scope: tenantScope("nolab")}, connect.CodePermissionDenied, ""},
		{"a tenant that is not ready", local, &v1.ExportAuditRequest{Scope: tenantScope("down")}, connect.CodeUnavailable, ""},
	} {
		r := exportAll(f.as(c.as), c.req)
		var ce *connect.Error
		if connect.CodeOf(r.err) != c.code || len(r.msgs) != 0 || (c.field != "" && (!errors.As(r.err, &ce) || !strings.Contains(ce.Message(), c.field))) {
			t.Errorf("%s: %v with %d messages, want %v naming %q", c.name, r.err, len(r.msgs), c.code, c.field)
		}
	}

	// A tenant's log, by its admin, in its own log.
	ten := exportAll(f.as(f.lab1Admin), &v1.ExportAuditRequest{Scope: tenantScope("lab1")})
	if ten.err != nil {
		t.Fatal(ten.err)
	}
	if th := ten.msgs[0].GetHeader(); th.GetTenant() != "lab1" || th.GetLog() != tenantLast.Log {
		t.Errorf("the tenant's header %v", th)
	}
	if got := entriesOf(f.tenantLog("lab1"), ten.requestID); len(got) != 2 {
		t.Errorf("%d entries of the tenant's export in its log, want 2", len(got))
	}
}
