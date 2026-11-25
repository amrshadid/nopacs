package storageswitch

import (
	"bytes"
	"context"
	"errors"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// takeOwner takes the owner record on the replica's place one generation
// past both the record found there and the generation the database holds
// for the replica's name, naming this installation and the tenant, signed
// with the owner keys: the swap moves that generation with the place to the
// primary's name, so the place every write goes to after it holds a record
// the database agrees with, and a server's check of its owner records does
// not fence the tenant. The database is written before the record, as every
// advance of a record is: a failure between the two leaves the record one
// generation behind, which a check accepts, never one past the database.
//
// A record on the place that names another installation or tenant, holds a
// later generation than the database, or names this installation without a
// MAC its keys make is not taken over: the place belongs to someone else.
func (s *Switcher) takeOwner(ctx context.Context, replica tenant.Storage) error {
	owners, err := s.cfg.DB.StorageOwners(ctx)
	if err != nil {
		return err
	}
	var held int64
	for _, o := range owners {
		if o.Storage == replica.Name {
			held = o.Generation
		}
	}
	bs, err := s.cfg.OpenPlace(ctx, replica)
	if err != nil {
		return err
	}
	defer func() { _ = bs.Close() }()
	gen := held
	rc, err := bs.Read(ctx, layout.OwnerKey, 0, -1)
	switch {
	case fault.KindOf(err) == fault.NotFound:
	case err != nil:
		return err
	default:
		o, derr := record.DecodeOwner(rc)
		_ = rc.Close()
		if err := s.judgeOwner(o, derr, held); err != nil {
			return err
		}
		gen = max(gen, o.Generation)
	}
	if gen >= record.MaxGeneration {
		return fault.New(fault.Invalid, "the replica's owner generation cannot grow past "+strconv.FormatInt(record.MaxGeneration, 10))
	}
	gen++
	now := s.now()
	if err := s.cfg.DB.SetStorageOwner(ctx, replica.Name, gen, now); err != nil {
		return err
	}
	b, err := record.EncodeOwner(record.Owner{Format: record.Format, Installation: s.cfg.Installation, Tenant: s.cfg.Tenant.ID,
		Generation: gen, Written: now}, s.cfg.OwnerKeys)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "the owner record cannot be made")
	}
	_, err = bs.Put(ctx, layout.OwnerKey, bytes.NewReader(b))
	return err
}

// judgeOwner returns an error unless the record o found on the replica's
// place (decodeErr when it did not decode) lets this installation write
// the tenant there at the generation held.
func (s *Switcher) judgeOwner(o record.Owner, decodeErr error, held int64) error {
	switch {
	case decodeErr != nil:
		return fault.Wrap(fault.Precondition, decodeErr, "the owner record on the replica's place is not one this installation writes")
	case o.Installation != s.cfg.Installation:
		return fault.New(fault.Precondition, "the owner record on the replica's place names another installation")
	case o.Tenant != s.cfg.Tenant.ID:
		return fault.New(fault.Precondition, "the owner record on the replica's place names another tenant")
	case o.Generation > held:
		return fault.New(fault.Precondition, "the owner record on the replica's place is at generation "+strconv.FormatInt(o.Generation, 10)+
			", past the "+strconv.FormatInt(held, 10)+" this tenant's database holds")
	}
	if err := record.VerifyOwner(o, s.cfg.OwnerKeys); err != nil {
		if errors.Is(err, record.ErrUnauthenticated) {
			return fault.Wrap(fault.Precondition, err, "the owner record on the replica's place names this installation but was not written by it")
		}
		return fault.Wrap(fault.Internal, err, "the owner record on the replica's place cannot be checked")
	}
	return nil
}
