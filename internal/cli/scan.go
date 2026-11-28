package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// splitRef reads "<subject>/<session>/<scan>" style references: between lo and
// hi non-empty parts separated by "/". A label never holds a slash (the label
// rule), so the split is unambiguous.
func splitRef(arg string, lo, hi int, shape string) ([]string, error) {
	parts := strings.Split(arg, "/")
	if len(parts) < lo || len(parts) > hi {
		return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not %s", output.Clean(arg), shape))
	}
	for _, p := range parts {
		if p == "" {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not %s: a part is empty", output.Clean(arg), shape))
		}
	}
	return parts, nil
}

// newScan builds the scan noun.
func (a *app) newScan() *cobra.Command {
	return group("scan", "Show a scan of the archive, and its DICOM headers",
		`A scan is a series of one session of one subject. Name it as
<subject>/<session>/<scan> after the tenant.`, a.newScanShow())
}

// scanDetails lists a scan's catalog row as labeled values.
func (a *app) scanDetails(s *v1.Scan) []output.Pair {
	return []output.Pair{
		{Label: "Subject", Value: s.GetSubjectLabel()},
		{Label: "Session", Value: s.GetSessionLabel()},
		{Label: "Scan", Value: s.GetLabel()},
		{Label: "Series UID", Value: output.Value(s.GetValuesWithheld(), s.GetSeriesInstanceUid())},
		{Label: "Modality", Value: output.Value(s.GetValuesWithheld(), s.GetModality())},
		{Label: "Series number", Value: seriesNumber(s)},
		{Label: "Description", Value: output.Value(s.GetValuesWithheld(), s.GetSeriesDescription())},
		{Label: "Instances", Value: count(s.GetInstanceCount())},
		{Label: "Size", Value: output.Human(s.GetSizeBytes())},
		{Label: "Storage", Value: output.Dash(s.GetStorage())},
		{Label: "Manifest SHA-256", Value: output.Dash(s.GetManifestSha256())},
		{Label: "Receiver", Value: output.Dash(s.GetReceiver())},
		{Label: "Archived", Value: a.when(s.GetArchiveTime())},
		{Label: "Updated", Value: a.when(s.GetUpdateTime())},
	}
}

// instanceJSON is one instance of "scan show --metadata --json": its DICOM
// JSON is an object, not a string holding JSON.
type instanceJSON struct {
	InstanceID        string `json:"instanceId"`
	SOPInstanceUID    string `json:"sopInstanceUid"`
	TransferSyntaxUID string `json:"transferSyntaxUid"`
	JSONTooLarge      bool   `json:"jsonTooLarge"`
	BulkDataOmitted   bool   `json:"bulkDataOmitted"`
	// Profile is the profile version that de-identified the instance,
	// "name@version"; left out for one archived as received.
	Profile   string          `json:"profile,omitempty"`
	DicomJSON json.RawMessage `json:"dicomJson"`
}

// profileOf writes the profile version an instance names as
// "name@version", or "" for none.
func profileOf(in *v1.InstanceMetadata) string {
	p := in.GetProfile()
	if p == nil {
		return ""
	}
	return p.GetName() + "@" + strconv.Itoa(int(p.GetVersion()))
}

// scanJSON is the document of "scan show --json".
type scanJSON struct {
	Scan           json.RawMessage `json:"scan"`
	ManifestSHA256 string          `json:"manifestSha256,omitempty"`
	Instances      []instanceJSON  `json:"instances,omitempty"`
}

// newScanShow builds "scan show".
func (a *app) newScanShow() *cobra.Command {
	var metadata bool
	cmd := &cobra.Command{
		Use:   "show <tenant> <subject>/<session>/<scan> [--metadata]",
		Short: "Show a scan's catalog row, and every instance's DICOM header on request",
		Long: `Show the catalog's row of one scan: its series, modality, counts, size,
storage and the SHA-256 of its manifest.

--metadata also shows the header of every instance of the scan, a page at a
time, in the DICOM JSON model (PS3.18 Annex F) with bulk data referred to by
URI. An instance whose header is longer than 1 MiB, or whose file is in the
deflated transfer syntax, is marked as such. With --json each header is a JSON
object under "dicomJson". Headers hold what the sender sent, identifying data
included.`,
		Example: `  nopacs scan show lab1 sub-3f9a/ses-81c2/scan-07d4
  nopacs scan show lab1 sub-3f9a/ses-81c2/scan-07d4 --metadata --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			ref, err := splitRef(args[1], 3, 3, "a scan: use <subject>/<session>/<scan>")
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetScanResponse, error) {
				return c.Catalog.GetScan(ctx, &v1.GetScanRequest{Tenant: args[0], SubjectLabel: ref[0], SessionLabel: ref[1], ScanLabel: ref[2]})
			})
			if err != nil {
				return err
			}
			readPages := func(each func(*v1.GetScanMetadataResponse) error) error {
				token := ""
				for {
					page, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetScanMetadataResponse, error) {
						return c.Metadata.GetScanMetadata(ctx, &v1.GetScanMetadataRequest{
							Tenant: args[0], SubjectLabel: ref[0], SessionLabel: ref[1], ScanLabel: ref[2], PageSize: 100, PageToken: token,
						})
					})
					if err != nil {
						return err
					}
					if err := each(page); err != nil {
						return err
					}
					if page.GetNextPageToken() == "" {
						return nil
					}
					if page.GetNextPageToken() == token {
						return fault.New(fault.Internal, "the server answered the same page token twice")
					}
					token = page.GetNextPageToken()
				}
			}
			if a.opts.json {
				doc := scanJSON{}
				opts := protojson.MarshalOptions{EmitUnpopulated: true}
				if doc.Scan, err = opts.Marshal(res.GetScan()); err != nil {
					return fault.Wrap(fault.Internal, err, "encoding the scan as JSON")
				}
				if metadata {
					doc.Instances = []instanceJSON{}
					if err := readPages(func(p *v1.GetScanMetadataResponse) error {
						doc.ManifestSHA256 = p.GetManifestSha256()
						for _, in := range p.GetInstances() {
							ij := instanceJSON{
								InstanceID: in.GetInstanceId(), SOPInstanceUID: in.GetSopInstanceUid(), TransferSyntaxUID: in.GetTransferSyntaxUid(),
								JSONTooLarge: in.GetJsonTooLarge(), BulkDataOmitted: in.GetBulkDataOmitted(), Profile: profileOf(in), DicomJSON: json.RawMessage("null"),
							}
							if in.GetDicomJson() != "" {
								ij.DicomJSON = json.RawMessage(in.GetDicomJson())
							}
							doc.Instances = append(doc.Instances, ij)
						}
						return nil
					}); err != nil {
						return err
					}
				}
				return a.write(func(w io.Writer) error { return output.JSONValue(w, doc) })
			}
			if err := a.write(func(w io.Writer) error { return output.Details(w, a.style(), a.scanDetails(res.GetScan())) }); err != nil {
				return err
			}
			if !metadata {
				return nil
			}
			return readPages(func(p *v1.GetScanMetadataResponse) error {
				return a.write(func(w io.Writer) error {
					for _, in := range p.GetInstances() {
						made := ""
						if p := profileOf(in); p != "" {
							made = ", de-identified by " + output.Clean(p)
						}
						if _, err := fmt.Fprintf(w, "\nInstance %s (transfer syntax %s%s)\n", output.Clean(in.GetInstanceId()), output.Clean(output.Dash(in.GetTransferSyntaxUid())), made); err != nil {
							return err
						}
						text := in.GetDicomJson()
						switch {
						case in.GetJsonTooLarge():
							text = "(header not shown: longer than 1 MiB)"
						case in.GetBulkDataOmitted():
							text += "\n(bulk data left out: the file is in the deflated transfer syntax; download the file)"
						}
						if _, err := fmt.Fprintln(w, output.Clean(text)); err != nil {
							return err
						}
					}
					return nil
				})
			})
		}),
	}
	cmd.Flags().BoolVar(&metadata, "metadata", false, "also show the DICOM header of every instance of the scan, as JSON")
	return cmd
}
