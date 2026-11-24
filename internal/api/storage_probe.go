package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/blob/s3"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The test object of TestStorage.
const (
	// probePrefix is where a test object is put: no archived file is under
	// it, since the archive's keys start with data/ or meta/
	// (internal/core/layout), so the test never touches data.
	probePrefix = "nopacs-test/"
	// probeSize is the test object's size: small enough to cost nothing on
	// any storage, large enough that a store that cuts or pads an object is
	// seen in the comparison.
	probeSize = 4 << 10
	// probeRemoveTimeout bounds the removal of the test object when the
	// call's own deadline has passed, so a storage that hung during the
	// write still has its object removed when it answers again soon.
	probeRemoveTimeout = 10 * time.Second
	// probeReadLimit bounds what is read back: a store that answers more
	// than was written is wrong, and is not read to its end.
	probeReadLimit = probeSize + 1
)

// probeResult is the outcome of one step of a test, before it becomes the
// contract's message.
type probeResult struct {
	step     v1.StorageTestStep
	outcome  v1.StorageTestOutcome
	duration time.Duration
	fault    v1.StorageFault
}

// message returns the result as the contract writes it.
func (r probeResult) message() *v1.StorageTestResult {
	m := &v1.StorageTestResult{Step: r.step, Outcome: r.outcome, Fault: r.fault}
	if r.outcome != v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_SKIPPED {
		m.Duration = durationpb.New(r.duration)
	}
	return m
}

// skipped is a step that was not tried.
func skipped(step v1.StorageTestStep) probeResult {
	return probeResult{step: step, outcome: v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_SKIPPED}
}

// timed runs one step and returns its result: passed, or failed with the
// fault faultOf gives for its error.
func timed(step v1.StorageTestStep, faultOf func(error) v1.StorageFault, run func() error) probeResult {
	start := time.Now()
	err := run()
	r := probeResult{step: step, duration: time.Since(start), outcome: v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_PASSED}
	if err != nil {
		r.outcome, r.fault = v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_FAILED, faultOf(err)
	}
	return r
}

// probe runs the steps of TestStorage on a storage, opened by open: write a
// small object of random bytes under probePrefix, read it back whole,
// compare the SHA-256 of what was read with that of what was written, and
// remove it. A step after a failed one is skipped, except the removal,
// which is tried whenever the write was tried, since a write that failed
// may still have left the object whole (blob.Store.Put). Every failure is
// one of the contract's fixed faults (faultOf); no text of the storage's is
// kept. The steps run within ctx, the call's deadline; the removal, when
// that deadline has passed, gets probeRemoveTimeout of its own. A failure
// to open the storage is the write's failure: nothing was written, so
// nothing is removed.
func probe(ctx context.Context, faultOf func(error) v1.StorageFault, open func(context.Context) (blob.Store, error)) []probeResult {
	write, read, compare, remove := v1.StorageTestStep_STORAGE_TEST_STEP_WRITE, v1.StorageTestStep_STORAGE_TEST_STEP_READ,
		v1.StorageTestStep_STORAGE_TEST_STEP_COMPARE, v1.StorageTestStep_STORAGE_TEST_STEP_REMOVE
	var store blob.Store
	opened := timed(write, faultOf, func() error {
		var err error
		store, err = open(ctx)
		return err
	})
	if opened.outcome != v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_PASSED {
		return []probeResult{opened, skipped(read), skipped(compare), skipped(remove)}
	}
	defer func() { _ = store.Close() }()

	body := make([]byte, probeSize)
	_, _ = rand.Read(body) // crypto/rand.Read never fails (Go 1.24).
	name := make([]byte, 16)
	_, _ = rand.Read(name)
	key := probePrefix + hex.EncodeToString(name)
	want := sha256.Sum256(body)

	results := make([]probeResult, 0, 4)
	w := timed(write, faultOf, func() error {
		_, err := store.Put(ctx, key, bytes.NewReader(body))
		return err
	})
	w.duration += opened.duration
	results = append(results, w)
	if w.outcome == v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_PASSED {
		var got []byte
		r := timed(read, faultOf, func() error {
			rc, err := store.Read(ctx, key, 0, -1)
			if err != nil {
				return err
			}
			defer func() { _ = rc.Close() }()
			got, err = io.ReadAll(io.LimitReader(rc, probeReadLimit))
			return err
		})
		results = append(results, r)
		if r.outcome == v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_PASSED {
			results = append(results, timed(compare, faultOf, func() error {
				if sha256.Sum256(got) != want {
					return errWrongAnswer
				}
				return nil
			}))
		} else {
			results = append(results, skipped(compare))
		}
	} else {
		results = append(results, skipped(read), skipped(compare))
	}
	rctx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), probeRemoveTimeout)
		defer cancel()
	}
	results = append(results, timed(remove, faultOf, func() error { return store.Delete(rctx, key) }))
	return results
}

// errWrongAnswer is the failure of a comparison: what was read is not what
// was written.
var errWrongAnswer = errors.New("what was read back is not what was written")

// directoryFault returns the contract's fault for an error of a directory
// storage. The directory driver keeps the file system's error in its chain
// (with no path in it) and marks a directory it could not reach at all with
// dir.ErrUnreachable, so causes are read, never words: a directory not there
// is not found; a refusal of permission, or any other failure of the file
// system once the directory is open, is the file system refusing the
// operation; a directory that could not be examined or opened at all is
// unreachable.
func directoryFault(err error) v1.StorageFault {
	switch {
	case errors.Is(err, errWrongAnswer):
		return v1.StorageFault_STORAGE_FAULT_WRONG_ANSWER
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return v1.StorageFault_STORAGE_FAULT_UNREACHABLE
	case fault.KindOf(err) == fault.NotFound || errors.Is(err, fs.ErrNotExist):
		return v1.StorageFault_STORAGE_FAULT_NOT_FOUND
	case errors.Is(err, fs.ErrPermission), fault.KindOf(err) == fault.Precondition:
		return v1.StorageFault_STORAGE_FAULT_REFUSED
	case errors.Is(err, dir.ErrUnreachable):
		return v1.StorageFault_STORAGE_FAULT_UNREACHABLE
	}
	return v1.StorageFault_STORAGE_FAULT_REFUSED
}

// s3Fault returns the contract's fault for an error of an S3 storage. The
// S3 driver keeps none of the SDK's or the network's text, and marks each
// cause a caller tells apart with one of its classes (internal/blob/s3,
// errors.go), which are read here, never its words; what it marks with none
// is a store that could not be reached or did not answer in time. A TLS
// failure is among what could not be reached, so it is answered
// unreachable: TLS_FAILED needs the driver to tell it apart.
func s3Fault(err error) v1.StorageFault {
	switch {
	case errors.Is(err, errWrongAnswer):
		return v1.StorageFault_STORAGE_FAULT_WRONG_ANSWER
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return v1.StorageFault_STORAGE_FAULT_UNREACHABLE
	case errors.Is(err, s3.ErrAddressNotAllowed):
		return v1.StorageFault_STORAGE_FAULT_ADDRESS_NOT_ALLOWED
	case fault.KindOf(err) == fault.NotFound, errors.Is(err, s3.ErrBucketMissing):
		return v1.StorageFault_STORAGE_FAULT_NOT_FOUND
	case errors.Is(err, s3.ErrRefused), fault.KindOf(err) == fault.Precondition:
		return v1.StorageFault_STORAGE_FAULT_REFUSED
	case errors.Is(err, s3.ErrWrongAnswer), fault.KindOf(err) == fault.Invalid:
		return v1.StorageFault_STORAGE_FAULT_WRONG_ANSWER
	}
	return v1.StorageFault_STORAGE_FAULT_UNREACHABLE
}

// faultOfType returns the classifier of a storage type's errors.
func faultOfType(t tenant.StorageType) func(error) v1.StorageFault {
	if t == tenant.StorageS3 {
		return s3Fault
	}
	return directoryFault
}

// passedAll reports whether every result passed.
func passedAll(results []probeResult) bool {
	for _, r := range results {
		if r.outcome != v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_PASSED {
			return false
		}
	}
	return len(results) > 0
}
