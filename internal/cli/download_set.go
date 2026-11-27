package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/download"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// downloadSet is the flags that choose what "download" fetches besides the
// references named after the tenant: a file of references, and the filters of
// "catalog scans". Every way of choosing ends as the same list of references
// (download.ReadRefs's form), so a search that comes later adds a way of
// making that list and changes nothing downstream.
type downloadSet struct {
	refsFile                     string
	modality, description        string
	receiver                     string
	archivedSince, archivedUntil string
	selection                    string
}

// add registers the flags.
func (s *downloadSet) add(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&s.refsFile, "refs-file", "", "a file of references to download, one <subject>[/<session>[/<scan>]] per line, blank lines and lines starting with # ignored; - reads standard input")
	f.StringVar(&s.modality, "modality", "", "download every scan of this modality in the tenant, such as MR (the filter of catalog scans; names no reference)")
	f.StringVar(&s.description, "description-prefix", "", "download every scan whose series description starts with this, ignoring case (the filter of catalog scans; names no reference)")
	f.StringVar(&s.receiver, "receiver", "", "download every scan first archived through the receiver of this name (the filter of catalog scans; names no reference)")
	f.StringVar(&s.archivedSince, "archived-since", "", "download only scans first archived at or after this time: a date, a time with its offset, or an age such as 7d (with a scan filter)")
	f.StringVar(&s.archivedUntil, "archived-until", "", "download only scans first archived before this time, written like --archived-since (with a scan filter)")
	f.StringVar(&s.selection, "selection", "", "the file that records which files are whole and checked, written as the download goes; the same command with the same file skips them without reading them again")
}

// filtered says whether a scan filter was given.
func (s *downloadSet) filtered() bool {
	return s.modality != "" || s.description != "" || s.receiver != "" || s.archivedSince != "" || s.archivedUntil != ""
}

// refs resolves the references of a download: those named after the tenant,
// those in the file, or the scans a filter selects, or, when none of these is
// given, those a selection file already holds.
func (a *app) downloadRefs(ctx context.Context, cmd *cobra.Command, c *client.Client, tenant string, named []string, s *downloadSet) ([][]string, error) {
	var refs [][]string
	for _, arg := range named {
		ref, err := download.ParseRef(arg)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if s.refsFile != "" {
		r, err := a.readRefsFile(s.refsFile)
		if err != nil {
			return nil, err
		}
		refs = append(refs, r...)
	}
	if s.filtered() {
		if len(refs) > 0 {
			return nil, fault.New(fault.Invalid, "a scan filter selects scans of the whole tenant: name no references with it")
		}
		return a.filteredScans(ctx, cmd, c, tenant, s)
	}
	if len(refs) > 0 {
		return refs, nil
	}
	if s.selection != "" {
		t, _, saved, found, err := download.SelectionRefs(s.selection)
		if err != nil {
			return nil, fault.Wrap(fault.Invalid, err, err.Error())
		}
		if found {
			if t != tenant {
				return nil, fault.New(fault.Invalid, fmt.Sprintf("the selection file %s was made for tenant %q, not %q", output.Clean(s.selection), output.Clean(t), output.Clean(tenant)))
			}
			return saved, nil
		}
	}
	return nil, fault.New(fault.Invalid, "name what to download: a <subject>[/<session>[/<scan>]], --refs-file, a scan filter such as --modality, or a --selection file from an earlier run")
}

// readRefsFile reads a list of references from a file, or from standard input
// for "-".
func (a *app) readRefsFile(path string) ([][]string, error) {
	r := a.env.Stdin
	if path == "-" && r == nil {
		return nil, fault.New(fault.Invalid, "--refs-file - reads standard input, and there is none")
	}
	if path != "-" {
		f, err := os.Open(path) // #nosec G304 -- the file the person named with --refs-file
		if err != nil {
			return nil, fault.Wrap(fault.Invalid, err, "cannot read "+output.Clean(path)+": "+err.Error())
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	refs, err := download.ReadRefs(r)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, fault.New(fault.Invalid, output.Clean(path)+" names no reference")
	}
	return refs, nil
}

// filteredScans lists every scan the filters of "catalog scans" select, page
// after page, as references of three labels.
func (a *app) filteredScans(ctx context.Context, cmd *cobra.Command, c *client.Client, tenant string, s *downloadSet) ([][]string, error) {
	var from, to *timestamppb.Timestamp
	now := a.env.Now()
	if s.archivedSince != "" {
		t, err := parseTime("archived-since", s.archivedSince, now, a.env.Location)
		if err != nil {
			return nil, err
		}
		from = timestamppb.New(t)
	}
	if s.archivedUntil != "" {
		t, err := parseTime("archived-until", s.archivedUntil, now, a.env.Location)
		if err != nil {
			return nil, err
		}
		to = timestamppb.New(t)
	}
	if from != nil && to != nil && !to.AsTime().After(from.AsTime()) {
		return nil, fault.New(fault.Invalid, "--archived-until must be after --archived-since")
	}
	order := scansOrder("", "", s.modality, s.receiver, from != nil || to != nil)
	if s.description != "" {
		order = v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION
	}
	scans, _, err := fetchPages(ctx, listFlags{all: true}, func(ctx context.Context, size int32, token string) ([]*v1.Scan, string, error) {
		res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListScansResponse, error) {
			return c.Catalog.ListScans(ctx, &v1.ListScansRequest{
				Tenant: tenant, Modality: s.modality, SeriesDescriptionPrefix: s.description, Receiver: s.receiver,
				StartTime: from, EndTime: to, Order: order, PageSize: size, PageToken: token,
			})
		})
		if err != nil {
			return nil, "", err
		}
		return res.GetScans(), res.GetNextPageToken(), nil
	})
	if err != nil {
		return nil, err
	}
	if len(scans) == 0 {
		return nil, fault.New(fault.NotFound, "no scan of the tenant matches the filters")
	}
	refs := make([][]string, 0, len(scans))
	for _, sc := range scans {
		ref := []string{sc.GetSubjectLabel(), sc.GetSessionLabel(), sc.GetLabel()}
		if _, err := download.ParseRef(strings.Join(ref, "/")); err != nil {
			return nil, fault.New(fault.Internal, "the server listed a scan under a name that is not a label: "+output.Clean(strings.Join(ref, "/")))
		}
		refs = append(refs, ref)
	}
	return refs, nil
}
