package api_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// TestMetadataOfADeflatedFile guards the deflated transfer syntax, where no
// offset in the file names a value: the instance is answered with every
// value that would be bulk data left out, attribute and all, and
// bulk_data_omitted set, while the rest of its header is answered.
func TestMetadataOfADeflatedFile(t *testing.T) {
	file := deflatedFile(t, corpusFile(t, "MR_small.dcm"), '0')
	store, path, held, line := storedInstance(t, file)
	got, err := api.MetaInstance(context.Background(), store, "lab1", path, 1<<20, held, line)
	if err != nil {
		t.Fatal(err)
	}
	set := dataset(t, got.GetDicomJson())
	if got.GetTransferSyntaxUid() != "1.2.840.10008.1.2.1.99" || !got.GetBulkDataOmitted() || got.GetJsonTooLarge() {
		t.Errorf("transfer syntax %q, omitted %v, too large %v", got.GetTransferSyntaxUid(), got.GetBulkDataOmitted(), got.GetJsonTooLarge())
	}
	if set["7FE00010"] != nil || strings.Contains(got.GetDicomJson(), "BulkDataURI") {
		t.Errorf("a deflated file's answer refers to bulk data: %.200s", got.GetDicomJson())
	}
	if set["00100020"] == nil || set["00280010"] == nil {
		t.Errorf("the rest of the header is not answered: %.200s", got.GetDicomJson())
	}
}

// TestDeflatedFileIsReceivedArchivedAndAnswered guards a deflated file going
// through real reception: its pixel data lies at a place counted in inflated
// bytes, which the scan's metadata file cannot hold, so the file must still
// end archived byte for byte (not retried for ever) with no pixel data place
// and its transfer syntax recorded, and the service must answer it with bulk
// data omitted.
func TestDeflatedFileIsReceivedArchivedAndAnswered(t *testing.T) {
	file := deflatedFile(t, corpusFile(t, "MR_small.dcm"), '0')
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, [][]byte{file})
	scans := a.scans(t)
	if len(scans) != 1 {
		t.Fatalf("%d scans, want 1", len(scans))
	}
	held := a.held(t, scans[0])
	if len(held) != 1 {
		t.Fatalf("%d instances held, want 1", len(held))
	}
	if got := a.object(t, held[0].Key); !bytes.Equal(got, file) {
		t.Errorf("the archived file is %d bytes and differs from the %d bytes received", len(got), len(file))
	}
	key, err := layout.MetadataKey(scans[0])
	if err != nil {
		t.Fatal(err)
	}
	rd := sidecar.NewReader(bytes.NewReader(a.object(t, key)))
	in, _, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if in.PixelData != nil || in.TransferSyntax != "1.2.840.10008.1.2.1.99" {
		t.Errorf("the metadata file's line has pixel data %+v and transfer syntax %q, want none and the deflated syntax", in.PixelData, in.TransferSyntax)
	}
	md := f.as(f.lab1Reader).metadata
	res := mustCall[*v1.GetScanMetadataResponse](t, "scan", call(md.GetScanMetadata, scanRequest(scans[0])))
	if len(res.GetInstances()) != 1 {
		t.Fatalf("%d instances answered, want 1", len(res.GetInstances()))
	}
	m := res.GetInstances()[0]
	if m.GetTransferSyntaxUid() != "1.2.840.10008.1.2.1.99" || !m.GetBulkDataOmitted() || m.GetJsonTooLarge() {
		t.Errorf("transfer syntax %q, omitted %v, too large %v", m.GetTransferSyntaxUid(), m.GetBulkDataOmitted(), m.GetJsonTooLarge())
	}
	if strings.Contains(m.GetDicomJson(), "BulkDataURI") || dataset(t, m.GetDicomJson())["00100020"] == nil {
		t.Errorf("the answer refers to bulk data or lacks the header: %.200s", m.GetDicomJson())
	}
}
