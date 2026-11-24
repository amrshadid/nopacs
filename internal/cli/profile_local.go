package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/trial"
)

// localProfile is the profile a local trial runs: expanded here, with the
// method its files record and the version it was exported from, if any.
type localProfile struct {
	expanded *v1.ExpandedProfile
	checksum string
	method   string
	version  *v1.ProfileVersion
}

// readLocalProfile reads the profile of a local trial from a file, or from
// standard input for "-": a document in its file form, or a version as
// "profile export --json" writes it (the version's answer, its document
// within). The document is checked and expanded by this build's language;
// an export whose checksum this build does not reach is refused, since the
// trial would not be of that version.
func (a *app) readLocalProfile(from string) (localProfile, error) {
	var b []byte
	var err error
	if from == "-" {
		if a.env.Stdin == nil {
			return localProfile{}, fault.New(fault.Invalid, "--document - has no standard input to read")
		}
		b, err = io.ReadAll(io.LimitReader(a.env.Stdin, profile.MaxDocumentBytes+1))
	} else {
		var f *os.File
		if f, err = os.Open(from); err == nil { // #nosec G304 -- the person names the document to read.
			b, err = io.ReadAll(io.LimitReader(f, profile.MaxDocumentBytes*2))
			_ = f.Close()
		}
	}
	if err != nil {
		return localProfile{}, fault.Wrap(fault.Invalid, err, "cannot read --document "+from)
	}
	var out localProfile
	doc, derr := profile.ParseDocument(b)
	if derr != nil {
		var exported v1.GetProfileVersionResponse
		if protojson.Unmarshal(withoutSchemaVersion(b), &exported) != nil || exported.GetVersion().GetDocument() == nil {
			return localProfile{}, fault.Wrap(fault.Invalid, derr, strings.TrimPrefix(derr.Error(), "profile: ")+
				" (in --document "+from+", which is neither a document nor a version's export)")
		}
		v := exported.GetVersion()
		doc = v.GetDocument()
		out.version = &v1.ProfileVersion{Name: v.GetName(), Version: v.GetVersion(), Checksum: v.GetChecksum(), CreateTime: v.GetCreateTime(),
			CreatedByKeyId: v.GetCreatedByKeyId(), Note: v.GetNote()}
	}
	e, ps := profile.Expand(doc, profile.Shipped())
	if len(ps) > 0 {
		_ = a.write(func(w io.Writer) error { return problemsTable(w, a, problemsList(ps)) })
		return localProfile{}, fault.New(fault.Invalid, fmt.Sprintf("the document has %d problem(s)", len(ps)))
	}
	_, sum, err := profile.Checksum(e)
	if err != nil {
		return localProfile{}, fault.Wrap(fault.Internal, err, "the checksum of a valid document: "+err.Error())
	}
	out.expanded, out.checksum, out.method = e, sum, "unsaved "+sum[:16]
	if v := out.version; v != nil {
		if v.GetChecksum() != "" && v.GetChecksum() != sum {
			return localProfile{}, fault.New(fault.Invalid, fmt.Sprintf("--document %s is %s@%d, whose checksum this build does not reach (another language edition); try it on a server",
				from, output.Clean(v.GetName()), v.GetVersion()))
		}
		out.method = v.GetName() + "@" + strconv.Itoa(int(v.GetVersion()))
	}
	return out, nil
}

// withoutSchemaVersion returns a JSON object without the "schemaVersion"
// the command line's --json writes first, so the answer it wrote reads
// back as the API's message; anything else is returned as it is.
func withoutSchemaVersion(b []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	if _, ok := m["schemaVersion"]; !ok {
		return b
	}
	delete(m, "schemaVersion")
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return out
}

// problemsList returns the language's problems as the API writes them.
func problemsList(ps profile.Problems) []*v1.ProfileProblem {
	out := make([]*v1.ProfileProblem, len(ps))
	for i, p := range ps {
		out[i] = p.Proto()
	}
	return out
}

// tryLocal runs a trial in this process: no server, no tenant, nothing
// sent; each file read in turn, at most trial.MaxHeldBytes.
func (a *app) tryLocal(ctx context.Context, f *trialProfileFlags, paths []string) (*v1.TryProfileResponse, error) {
	if f.document == "" {
		return nil, fault.New(fault.Invalid, "--local needs --document: a document, or a version's export")
	}
	p, err := a.readLocalProfile(f.document)
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	var names []string
	err = walkFiles(paths, func(path string, size int64) error {
		if size == 0 || size > trial.MaxHeldBytes {
			return fault.New(fault.Invalid, fmt.Sprintf("%s is %s; a local trial reads files of 1 byte to 64MiB", path, output.Bytes(size)))
		}
		if _, seen := sizes[path]; !seen {
			names = append(names, path)
		}
		sizes[path] = size
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fault.New(fault.Invalid, "name at least one file, or a directory that holds DICOM files")
	}
	return trial.TryLocal(ctx, trial.LocalTrial{
		Expanded: p.expanded, Checksum: p.checksum, Method: p.method, Version: p.version,
		Reception: engine.Reception{CallingAETitle: f.calling, CalledAETitle: f.called},
		Names:     names,
		Read:      func(name string) ([]byte, error) { return readBounded(name, sizes[name]) },
	})
}
