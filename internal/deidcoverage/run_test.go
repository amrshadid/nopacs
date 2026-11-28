package deidcoverage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// oneScan is a coverage store of one scan that keeps the page written.
type oneScan struct {
	tenant.DeidCoverages
	scan  tenant.DeidCoverageScan
	given bool
	apply tenant.DeidCoverageApply
}

func (o *oneScan) DeidCoverageJob(context.Context, string) (tenant.DeidCoverageJob, error) {
	return tenant.DeidCoverageJob{Job: tenant.Job{State: tenant.JobQueued}}, nil
}

func (o *oneScan) DeidCoverageScans(context.Context, int64, int) ([]tenant.DeidCoverageScan, error) {
	if o.given {
		return nil, nil
	}
	o.given = true
	return []tenant.DeidCoverageScan{o.scan}, nil
}

func (o *oneScan) ApplyDeidCoverage(_ context.Context, a tenant.DeidCoverageApply) (tenant.DeidCoverageJob, error) {
	o.apply = a
	return tenant.DeidCoverageJob{After: a.Reached()}, nil
}

func (o *oneScan) EndDeidCoverage(context.Context, string, time.Time) (tenant.Job, error) {
	return tenant.Job{State: tenant.JobDone}, nil
}

// bytesStore is a storage holding one object.
type bytesStore struct {
	blob.Store
	data []byte
}

func (b bytesStore) Read(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.data)), nil
}

type bytesStorages struct{ st blob.Store }

func (b bytesStorages) Get(context.Context, string) (blob.Store, error) { return b.st, nil }
func (bytesStorages) Close()                                            {}

// TestAScanHoldsAtMostTheBoundOfElementsInMemory guards the memory of one
// scan: a scan whose lines list far more distinct private creators than a
// report holds is counted with at most tenant.MaxCoverageElements elements,
// the occurrences past it counted as not listed, never kept.
func TestAScanHoldsAtMostTheBoundOfElementsInMemory(t *testing.T) {
	path := label.Path{Subject: "s", Session: "e", Scan: "c"}
	lines := tenant.MaxCoverageElements/sidecar.MaxUnnamed + 10
	var buf bytes.Buffer
	w := sidecar.NewWriterV3(&buf, sidecar.ScanHeader{StudyUID: "1.2.3", SeriesUID: "1.2.3.4", Receiver: "r", SessionReceiver: "r",
		Created: time.Unix(1, 0).UTC(), Updated: time.Unix(1, 0).UTC()})
	for i := range lines {
		uid := fmt.Sprintf("1.2.3.4.1%06d", i)
		key, err := layout.InstanceKey(path, uid)
		if err != nil {
			t.Fatal(err)
		}
		in := sidecar.Instance{SOPInstanceUID: uid, Key: key, Size: 10, SHA256: strings.Repeat("ab", 32), Dataset: json.RawMessage(`{}`),
			Reception: "rcp", HeldAt: time.Unix(1, 0).UTC(), Profile: "research@1"}
		for j := range sidecar.MaxUnnamed {
			in.Unnamed = append(in.Unnamed, sidecar.UnnamedElement{Tag: 0x00290001, VR: "LO", Count: 1, Creator: fmt.Sprintf("C%d-%d", i, j)})
		}
		if err := w.Add(in); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	store := &oneScan{scan: tenant.DeidCoverageScan{ID: 1, Path: path, Storage: "main"}}
	r, err := New(Config{Store: store, Now: time.Now, Storages: func(context.Context) (Storages, error) {
		return bytesStorages{bytesStore{data: buf.Bytes()}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.Run(context.Background(), "job"); err != nil || !res.Ended {
		t.Fatalf("the pass: ended %v, %v", res.Ended, err)
	}
	if r.seams.largestScan > tenant.MaxCoverageElements {
		t.Errorf("a scan held %d distinct elements in memory, more than the bound %d", r.seams.largestScan, tenant.MaxCoverageElements)
	}
	if n := len(store.apply.Elements); n != tenant.MaxCoverageElements {
		t.Errorf("the page lists %d elements, want the bound %d", n, tenant.MaxCoverageElements)
	}
	if got := store.apply.Scans[0].NotListed; got != int64(lines*sidecar.MaxUnnamed-tenant.MaxCoverageElements) {
		t.Errorf("%d occurrences counted as not listed, want %d", got, lines*sidecar.MaxUnnamed-tenant.MaxCoverageElements)
	}
}
