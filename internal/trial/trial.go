// Package trial is what a trial run of a de-identification profile shares
// between the server, which tries files for a caller, and the command line,
// which tries them in its own process with no server: the lookups that assign
// nothing, the throwaway keys, the answer for one file and the counts of a
// run. It holds no database, no storage and no tenant, so the command line can
// link it.
package trial

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
)

// MaxHeldBytes bounds one file a trial run reads into memory, held on a
// server or read by the command line.
const MaxHeldBytes = 64 << 20

// UnassignedCode is the code a trial run answers for a value its lookup
// table has none for, where a receiver would assign the table's next: a
// trial assigns nothing. It has the form of a code (tenant.ValidLookupCode).
const UnassignedCode = "UNASSIGNED"

// Lookups answers a trial run's lookups without reading a tenant's
// tables: every value is answered UnassignedCode and counted, and a value
// a receiver would hold the file for when missing is reported missing, so
// the verdict says it depends on the table. A trial that read the tables
// would tell its caller which values have codes, and which codes.
type Lookups struct {
	// Unassigned counts the values answered UnassignedCode.
	Unassigned *int
}

// Code reports every value as having no code.
func (l Lookups) Code(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// Assign answers UnassignedCode for every key; nothing is read or
// assigned.
func (l Lookups) Assign(_ context.Context, keys []engine.LookupKey) ([]string, error) {
	out := make([]string, len(keys))
	for i := range keys {
		out[i] = UnassignedCode
		*l.Unassigned++
	}
	return out, nil
}

// ThrowawayKeys makes the keys a trial runs under: three random keys of
// its own, never a tenant's, so a remapped UID, a hash, a shifted date or
// a label a trial answers can never be matched with what the archive
// holds.
func ThrowawayKeys() (engine.Keys, error) {
	var k engine.Keys
	for _, b := range []*[]byte{&k.UID, &k.Date, &k.Hash} {
		*b = make([]byte, engine.MinKeyLength)
		if _, err := rand.Read(*b); err != nil {
			return engine.Keys{}, fault.New(fault.Internal, "trial: no random source for a trial's keys")
		}
	}
	return k, nil
}

// NewEngine compiles the expanded profile under keys made for this run, its
// lookups never reading a table. A profile this build cannot run is a
// precondition failure.
func NewEngine(expanded *v1.ExpandedProfile, method string, unassigned *int) (*engine.Engine, engine.Keys, error) {
	keys, err := ThrowawayKeys()
	if err != nil {
		return nil, keys, err
	}
	en, err := engine.New(expanded, keys, engine.Options{Lookups: Lookups{Unassigned: unassigned}, Method: method})
	if err != nil {
		return nil, keys, fault.New(fault.Precondition, "profile: this server cannot run the profile: "+ReceptionRefusal(err))
	}
	return en, keys, nil
}

// SafeError returns err with its kind and its message only: what a trial
// fails with is answered and logged without anything it wraps, which could
// hold a file's value.
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	return fault.New(fault.KindOf(err), fault.Message(err))
}

// ReceptionRefusal says in plain words why a profile version cannot
// de-identify a receiver's files (engine.ForReception).
func ReceptionRefusal(err error) string {
	switch {
	case errors.Is(err, engine.ErrKeepsPlacingUIDs):
		return "it does not remap the UIDs a file is placed by (as the Retain UIDs option does), and a receiver places each file by their keyed remapping (" +
			strings.TrimPrefix(err.Error(), engine.ErrKeepsPlacingUIDs.Error()+": ") + ")"
	}
	return strings.TrimPrefix(err.Error(), "engine: ")
}
