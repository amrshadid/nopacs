package fakeserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strconv"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/core/auditchain"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The fake's audit seal: every log is sealed in FakeCheckpoints checkpoints
// of FakeCheckpointEntries entries each, made up, signed by nobody: what a
// command shows and pages through, not a chain anyone checks.
const (
	// FakeInstallation is the installation id the fake answers.
	FakeInstallation = "inst-fake-1"
	// FakeCheckpoints is how many checkpoints each log has.
	FakeCheckpoints = 3
	// FakeCheckpointEntries is how many entries each checkpoint seals.
	FakeCheckpointEntries = 4
)

// sealState is what a test set of the fake's audit seal.
type sealState struct {
	// verify, when set, is the answer of every VerifyAudit.
	verify *v1.VerifyAuditResponse
	// verifySteps, when above 0, makes VerifyAudit answer a next_seq that
	// many times before the last answer, as a server verifying a long log
	// in several calls does.
	verifySteps int
	// verifyQueue are answers VerifyAudit gives one a call, in order,
	// before any other.
	verifyQueue []*v1.VerifyAuditResponse
	// verifyRequests are the VerifyAudit requests received.
	verifyRequests []*v1.VerifyAuditRequest
	// exportHook, when set, runs in each ExportAudit after the header is
	// sent and before the entries: what a test does while an export is
	// being written.
	exportHook func()
}

// SetExportHook makes each ExportAudit run f after its header is sent and
// before its entries; nil for none.
func (s *Server) SetExportHook(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seal.exportHook = f
}

// SetVerifyAnswer makes every VerifyAudit answer v, after steps answers
// that each go on to the next seq, as a long log's do.
func (s *Server) SetVerifyAnswer(v *v1.VerifyAuditResponse, steps int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seal.verify, s.seal.verifySteps = v, steps
}

// QueueVerifyAnswers makes the next VerifyAudit calls answer each of
// answers in turn, before the answer SetVerifyAnswer set.
func (s *Server) QueueVerifyAnswers(answers ...*v1.VerifyAuditResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seal.verifyQueue = append(s.seal.verifyQueue, answers...)
}

// VerifyRequests returns the VerifyAudit requests the fake received.
func (s *Server) VerifyRequests() []*v1.VerifyAuditRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*v1.VerifyAuditRequest(nil), s.seal.verifyRequests...)
}

// fixedSealTime is when the fake's first checkpoint was made; each next
// one a day later.
var fixedSealTime = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

// fakeKey is the fake's audit public key: a fixed key, so its PEM is the
// same every run.
var fakeKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey)

// auditSealHandler implements AuditSealService.
type auditSealHandler struct{ s *Server }

var _ nopacsv1connect.AuditSealServiceHandler = auditSealHandler{}

// logOf checks a request's scope and returns the tenant it names, "" for
// the installation, and the log's name in its checkpoints.
func (h auditSealHandler) logOf(scope *v1.Scope) (string, string, error) {
	tenant, installation, err := scopeOf(scope)
	if err != nil {
		return "", "", err
	}
	if installation {
		return "", "master", nil
	}
	if err := h.s.checkTenant(tenant); err != nil {
		return "", "", err
	}
	return tenant, "tenant-id-" + tenant, nil
}

// fakeCheckpoint returns the fake's checkpoint number i, from 0.
func fakeCheckpoint(i int, tenant, log string) *v1.AuditCheckpoint {
	from := int64(i*FakeCheckpointEntries + 1)
	n := byte(i) //nolint:gosec // G115: i is below FakeCheckpoints.
	digest := bytes.Repeat([]byte{n + 1}, 32)
	previous := make([]byte, 32)
	if i > 0 {
		previous = bytes.Repeat([]byte{n}, 32)
	}
	return &v1.AuditCheckpoint{Tenant: tenant, FromSeq: from, ToSeq: from + FakeCheckpointEntries - 1, Entries: FakeCheckpointEntries,
		Head: bytes.Repeat([]byte{0xa0 + n}, 32), PreviousDigest: previous, Digest: digest,
		Time: timestamppb.New(fixedSealTime.AddDate(0, 0, i)), KeyVersion: 1, Signature: make([]byte, 64),
		InstallationId: FakeInstallation, Log: log}
}

// VerifyAudit answers the answer a test set, or intact over every entry.
func (h auditSealHandler) VerifyAudit(_ context.Context, req *v1.VerifyAuditRequest) (*v1.VerifyAuditResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	tenant, log, err := h.logOf(req.GetScope())
	if err != nil {
		return nil, err
	}
	if req.GetFromSeq() < 0 {
		return nil, fault.New(fault.Invalid, "from_seq: is 0 or more; 0 and 1 are the first entry")
	}
	if d := req.GetExpectedCheckpointDigest(); len(d) != 0 && len(d) != 32 {
		return nil, fault.New(fault.Invalid, "expected_checkpoint_digest: is empty or 32 bytes")
	}
	s.seal.verifyRequests = append(s.seal.verifyRequests, proto.Clone(req).(*v1.VerifyAuditRequest))
	last := fakeCheckpoint(FakeCheckpoints-1, tenant, log)
	if len(s.seal.verifyQueue) > 0 {
		next := s.seal.verifyQueue[0]
		s.seal.verifyQueue = s.seal.verifyQueue[1:]
		return proto.Clone(next).(*v1.VerifyAuditResponse), nil
	}
	if s.seal.verifySteps > 0 {
		s.seal.verifySteps--
		return &v1.VerifyAuditResponse{Intact: true, EntriesChecked: FakeCheckpointEntries, CheckpointsChecked: 1,
			NextSeq: max(req.GetFromSeq(), 1) + FakeCheckpointEntries, LastCheckpoint: last}, nil
	}
	if s.seal.verify != nil {
		return proto.Clone(s.seal.verify).(*v1.VerifyAuditResponse), nil
	}
	return &v1.VerifyAuditResponse{Intact: true, EntriesChecked: FakeCheckpoints * FakeCheckpointEntries,
		CheckpointsChecked: FakeCheckpoints, UnsealedEntries: 2, LastCheckpoint: last}, nil
}

// ListAuditCheckpoints answers a page of the fake's checkpoints, newest
// first.
func (h auditSealHandler) ListAuditCheckpoints(_ context.Context, req *v1.ListAuditCheckpointsRequest) (*v1.ListAuditCheckpointsResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	tenant, log, err := h.logOf(req.GetScope())
	if err != nil {
		return nil, err
	}
	all := make([]*v1.AuditCheckpoint, 0, FakeCheckpoints)
	for i := FakeCheckpoints - 1; i >= 0; i-- {
		all = append(all, fakeCheckpoint(i, tenant, log))
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListAuditCheckpointsResponse{Checkpoints: all[from:to], NextPageToken: next}, nil
}

// ExportAudit streams the fake's log: the header, then each checkpoint's
// made-up entries and the checkpoint, from the checkpoint that covers
// from_seq to the one that covers to_seq.
func (h auditSealHandler) ExportAudit(_ context.Context, req *v1.ExportAuditRequest, stream nopacsv1connect.AuditSealServiceExportAuditServerStream) error {
	s := h.s
	s.mu.Lock()
	tenant, log, err := h.logOf(req.GetScope())
	hook := s.seal.exportHook
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if req.GetFromSeq() < 0 || (req.ToSeq != nil && req.GetToSeq() < max(req.GetFromSeq(), 1)) {
		return fault.New(fault.Invalid, "from_seq: a range of seq is from a first seq to a last one not below it")
	}
	first := int(max(req.GetFromSeq(), 1)-1) / FakeCheckpointEntries
	if first >= FakeCheckpoints {
		first = FakeCheckpoints
	}
	pem, err := auditchain.MarshalPublicKey(fakeKey)
	if err != nil {
		return err
	}
	header := &v1.AuditExportHeader{InstallationId: FakeInstallation, Tenant: tenant, Log: log, EncodingVersion: 1, ChainBefore: make([]byte, 32),
		PublicKeys: []*v1.AuditPublicKey{{KeyVersion: 1, PublicKey: fakeKey, PublicKeyPem: string(pem), Current: true, InstallationId: FakeInstallation}}}
	if first < FakeCheckpoints {
		header.FromSeq = int64(first*FakeCheckpointEntries + 1)
	}
	if err := stream.Send(&v1.ExportAuditResponse{Kind: &v1.ExportAuditResponse_Header{Header: header}}); err != nil {
		return err
	}
	if hook != nil {
		hook()
	}
	for i := first; i < FakeCheckpoints; i++ {
		cp := fakeCheckpoint(i, tenant, log)
		if req.ToSeq != nil && cp.GetFromSeq() > req.GetToSeq() {
			return nil
		}
		for seq := cp.GetFromSeq(); seq <= cp.GetToSeq(); seq++ {
			e := &v1.SealedAuditEntry{Seq: seq, Encoded: []byte("entry " + strconv.FormatInt(seq, 10)), Chain: make([]byte, 32),
				Entry: &v1.AuditEntry{Id: "e" + strconv.FormatInt(seq, 10), Time: cp.GetTime(), Method: "/nopacs.v1.TenantService/ListTenants",
					LocalUserId: "501", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, RequestId: "req-" + strconv.FormatInt(seq, 10)}}
			if err := stream.Send(&v1.ExportAuditResponse{Kind: &v1.ExportAuditResponse_Entry{Entry: e}}); err != nil {
				return err
			}
		}
		if err := stream.Send(&v1.ExportAuditResponse{Kind: &v1.ExportAuditResponse_Checkpoint{Checkpoint: cp}}); err != nil {
			return err
		}
	}
	return nil
}

// GetAuditPublicKey answers the fake's one key.
func (h auditSealHandler) GetAuditPublicKey(context.Context, *v1.GetAuditPublicKeyRequest) (*v1.GetAuditPublicKeyResponse, error) {
	pem, err := auditchain.MarshalPublicKey(fakeKey)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "internal error")
	}
	return &v1.GetAuditPublicKeyResponse{InstallationId: FakeInstallation,
		PublicKeys: []*v1.AuditPublicKey{{KeyVersion: 1, PublicKey: fakeKey, PublicKeyPem: string(pem), Current: true, InstallationId: FakeInstallation}}}, nil
}
