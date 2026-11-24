package fakeserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// RotateKey replaces a key with a successor that holds its grants, holder
// and rules; the rotated key ends after the overlap, a day when none is
// given, or at its own expiry when that is sooner.
func (h accessHandler) RotateKey(_ context.Context, req *v1.RotateKeyRequest) (*v1.RotateKeyResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	k, err := s.findKey(req.GetId())
	if err != nil {
		return nil, err
	}
	switch {
	case k.key.GetRevokeTime() != nil:
		return nil, fault.New(fault.Conflict, "the key is revoked")
	case k.key.GetSuccessorId() != "":
		return nil, fault.New(fault.Conflict, "the key was already rotated")
	}
	overlap := 24 * time.Hour
	if o := req.GetOverlap(); o != nil {
		overlap = o.AsDuration()
	}
	if overlap < 0 || overlap > 30*24*time.Hour {
		return nil, fault.New(fault.Invalid, "a rotation's overlap is 0 to 30 days")
	}
	now := s.now()
	s.nextKey++
	id := fmt.Sprintf("k%011d", s.nextKey)
	next := proto.CloneOf(k.key)
	next.Id, next.CreateTime, next.LastUseTime, next.LastUseAddress, next.SuccessorId = id, timestamppb.New(now), nil, "", ""
	if old := k.key.GetExpireTime(); old != nil {
		next.ExpireTime = timestamppb.New(now.Add(old.AsTime().Sub(k.key.GetCreateTime().AsTime())))
	}
	ends := now.Add(overlap)
	if old := k.key.GetExpireTime(); old == nil || ends.Before(old.AsTime()) {
		k.key.ExpireTime = timestamppb.New(ends)
	}
	k.key.SuccessorId = id
	secret := "nopacs_" + id + "_" + strings.Repeat("R", 44)
	s.keys = append(s.keys, &keyRecord{key: next, secret: secret})
	for gk, role := range s.grants {
		if keyID, scope, _ := strings.Cut(gk, "|"); keyID == req.GetId() {
			s.grants[id+"|"+scope] = role
		}
	}
	return &v1.RotateKeyResponse{Key: next, Secret: secret, Rotated: k.key}, nil
}

// SetKeyRules replaces a key's networks and client certificate.
func (h accessHandler) SetKeyRules(_ context.Context, req *v1.SetKeyRulesRequest) (*v1.SetKeyRulesResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	k, err := s.findKey(req.GetId())
	if err != nil {
		return nil, err
	}
	if k.key.GetRevokeTime() != nil {
		return nil, fault.New(fault.Conflict, "the key is revoked")
	}
	k.key.Networks, k.key.ClientCertSha256 = req.GetNetworks(), req.GetClientCertSha256()
	return &v1.SetKeyRulesResponse{Key: k.key}, nil
}
