package fakeserver

import (
	"context"
	"fmt"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The fake's recall and sample: only the flows a command drives (a preview,
// its token, the recall it confirms, a sample drawn again from its seed),
// not the server's rules, which the API's own tests hold. Every target
// holds fakeRecallObjects objects of fakeRecallObjectBytes on the storage
// "cold", one of them being recalled already.
const (
	fakeRecallObjects     = 3
	fakeRecallObjectBytes = 1000
)

// fakeRecallToken is the token the fake's preview of a target gives.
func fakeRecallToken(req *v1.RecallRequest) string {
	return fmt.Sprintf("r1.%s/%s/%s.%d", req.GetSubjectLabel(), req.GetSessionLabel(), req.GetScanLabel(), req.GetDays())
}

// Recall answers a preview of a target with its token, and the recall a
// matching token confirms; a storage other than "cold" is not of the
// archive tier.
func (h replicaHandler) Recall(_ context.Context, req *v1.RecallRequest) (*v1.RecallResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if req.GetStorage() != "" && req.GetStorage() != "cold" {
		return nil, fault.New(fault.Precondition, "the storage "+req.GetStorage()+" is not of the archive tier")
	}
	out := &v1.RecallResponse{Storages: []string{"cold"}, Objects: fakeRecallObjects, Bytes: fakeRecallObjects * fakeRecallObjectBytes,
		ObjectsRecalling: 1, BytesRequested: (fakeRecallObjects - 1) * fakeRecallObjectBytes}
	if req.GetValidateOnly() {
		out.Confirm = fakeRecallToken(req)
		return out, nil
	}
	if req.GetConfirm() != fakeRecallToken(req) {
		return nil, fault.New(fault.Precondition, "the confirm token does not match this recall")
	}
	out.ObjectsRequested = fakeRecallObjects - 1
	return out, nil
}

// VerifySample answers count objects of the storage, the first of them
// chosen by the seed, so a seed draws the same sample again.
func (h replicaHandler) VerifySample(_ context.Context, req *v1.VerifySampleRequest) (*v1.VerifySampleResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, err := s.arch.findStorage(req.GetTenant(), req.GetStorage()); err != nil {
		return nil, err
	}
	out := &v1.VerifySampleResponse{Population: 1000}
	start := req.GetSeed() % 900
	if start < 0 {
		start = -start
	}
	for i := range int64(req.GetCount()) {
		n := start + i
		out.Objects = append(out.Objects, &v1.SampledObject{Key: fmt.Sprintf("data/sub-01/ses-01/t1w/1.2.3.%03d.dcm", n), SizeBytes: 1000 + n,
			Sha256: fmt.Sprintf("%064x", n), InstanceId: fmt.Sprintf("1.2.3.%03d", n), Revision: 1, SubjectLabel: "sub-01",
			SessionLabel: "ses-01", ScanLabel: "t1w"})
	}
	return out, nil
}
