package receive

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What it takes to run a newer profile version over a file already
// archived de-identified: the archived file is the only original there is,
// and what the version that made it decided is already done in it (a hash,
// a remapped UID, a shifted date). Running the newer version whole would do
// those again under other keys, so only what the newer version decides
// otherwise than the older one, and only where it lets through less, is
// done to the archived file.

// Tightening is one element a newer profile version takes more from.
type Tightening struct {
	// Path is where the element is, as engine.Entry.Path says.
	Path string
	// Tag is the element's tag.
	Tag uint32
	// Action is the newer version's action: a fixed word of the profile
	// language, never a value.
	Action string
}

// Tightened is what TightenFile worked out for one archived file.
type Tightened struct {
	// Written is the file with the tightenings made, its method record
	// naming the newer version, and read back by the check after; nil when
	// nothing changed or the file is held.
	Written []byte
	// Changes are the elements the newer version tightens, in the order the
	// data set is walked; empty when the file is as strict as the version
	// makes it.
	Changes []Tightening
	// Catalog are the values of the scan's catalog (Series Description,
	// Modality, Series Number) the tightenings change, as the new file holds
	// them; an element removed or emptied is given with an empty Value.
	Catalog []CatalogValue
	// Unnamed and UnnamedOver are the elements no rule of the newer version
	// named, as its run over the file found them, for the line of the new
	// revision (sidecar.Instance.Unnamed); empty but not nil when written.
	Unnamed     []sidecar.UnnamedElement
	UnnamedOver int
	// Hold is why the file may not be rewritten: the quarantine reason and
	// details a reception would hold it with, or the cause of a rewrite the
	// archive cannot make; nil otherwise.
	Hold *TrialHold
}

// CatalogValue is a catalog value of a scan as the new file holds it.
type CatalogValue struct {
	// Tag is the element's tag; Value its text, empty when it has none.
	Tag   uint32
	Value string
}

// TightenOptions are what TightenFile runs with besides its engines and file.
type TightenOptions struct {
	// HeaderLimit bounds the header, as receiver.header-limit does.
	HeaderLimit int64
	// Check makes the check after, as TrialOptions.Check does. Required.
	Check func(maxFileBytes int64) Checker
	// Memory is the server's bound on de-identification's memory, as
	// TrialOptions.Memory.
	Memory check.Memory
}

// The causes of a hold TightenFile makes of its own (the quarantine reason
// is deid_failed).
const (
	// TightenNeedsOriginal: the newer version transforms an element with a
	// key (a hash, a remapped UID, a shifted date, a lookup code) that the
	// older version left otherwise: the value would have to be made from the
	// original, which de-identified data does not keep.
	TightenNeedsOriginal = "rerun_needs_original"
	// TightenCatalogElement: the newer version changes an element the
	// catalog is built on (a UID, the study date, the character set, how the
	// pixel data is laid out), which a rewrite of one revision never changes.
	TightenCatalogElement = "rerun_catalog_element"
	// TightenCharset: the file's character set would not be kept by a
	// rewrite (CheckRewritable).
	TightenCharset = "rerun_charset"
	// TightenPixelRules: the newer version has pixel rules, which a rewrite
	// of the header does not apply.
	TightenPixelRules = "rerun_pixel_rules"
)

// tightening actions are those whose result does not depend on a key: what
// the older version already did, the newer one does over it.
var tighteningActions = map[string]bool{"remove": true, "empty": true, "replace": true, "set": true, "dummy": true,
	"remove_unless_required": true, "generalize": true, "clean": true, "redact": true, "create": true}

// rerunRecord are the elements the engine writes of itself; a rewrite takes
// them from the newer version's run and they are never a tightening.
var rerunRecord = map[uint32]bool{0x00120062: true, 0x00120063: true, 0x00120064: true, 0x00280303: true}

// catalogElements are the elements a rewrite of one revision never changes.
var catalogElements = map[uint32]bool{0x0020000D: true, 0x0020000E: true, 0x00080018: true, 0x00080016: true, 0x00080005: true,
	0x00080020: true, 0x00280002: true, 0x00280004: true, 0x00280006: true, 0x00280008: true, 0x00280010: true, 0x00280011: true,
	0x00280100: true, 0x00280101: true, 0x00280102: true, 0x00280103: true}

// TightenFile runs newer, a profile version compiled under keys of the
// caller's own, over file, an archived instance that older (the version its
// catalog row records) de-identified, and makes in the file what newer
// decides otherwise than older, where newer lets through less: elements it
// removes, empties, replaces by a constant, generalizes or cleans, and
// elements it adds. The check after runs over the new file, searching for
// the values taken out and for the version's text lists. An element newer
// transforms with a key, or one the catalog is built on, holds the file
// (Tightened.Hold) instead. A file newer changes nothing in comes back with
// no Changes and is not rewritten.
func TightenFile(ctx context.Context, newer, older *engine.Engine, file []byte, rec engine.Reception, o TightenOptions) (Tightened, error) {
	var t Tightened
	held := func(reason tenant.QuarantineReason, details map[string]string) (Tightened, error) {
		t.Written, t.Changes, t.Catalog = nil, nil, nil
		t.Hold = &TrialHold{Reason: reason, Details: details}
		return t, nil
	}
	h, err := header.ReadWithin(bytes.NewReader(file), int64(len(file)), o.HeaderLimit)
	if err != nil {
		return held(tenant.QuarantineNotDICOM, map[string]string{"cause": "unreadable"})
	}
	if err := CheckRewritable(file, o.HeaderLimit); err != nil {
		return held(tenant.QuarantineDeidFailed, map[string]string{"cause": TightenCharset})
	}
	read := func() (*dataset.Dataset, error) {
		ds, _, _, err := h.Dataset(bytes.NewReader(file))
		return ds, err
	}
	ds, source, littleEndian, err := h.Dataset(bytes.NewReader(file))
	if err != nil {
		return held(tenant.QuarantineDeidFailed, map[string]string{"cause": "unreadable"})
	}
	scratch, err := read()
	if err != nil {
		return held(tenant.QuarantineDeidFailed, map[string]string{"cause": "unreadable"})
	}
	old, err := read()
	if err != nil {
		return held(tenant.QuarantineDeidFailed, map[string]string{"cause": "unreadable"})
	}
	rec.CharacterSet = h.CharacterSet()
	if !rec.IdentityGiven {
		rec = withIdentity(rec, h, rec.Namespace)
	}
	oldRec, err := older.Apply(ctx, old, rec)
	var failure *engine.Failure
	switch {
	case errors.As(err, &failure):
		// The older version cannot be run over its own file: nothing of what
		// it did is known, so nothing is tightened.
		return held(tenant.QuarantineDeidFailed, map[string]string{"cause": "rerun_older_unusable"})
	case err != nil:
		return t, fault.New(fault.Unavailable, "a file could not be de-identified now")
	}
	was := make(map[string]string, len(oldRec.Entries))
	for _, e := range oldRec.Entries {
		was[e.Path] = e.Action
	}
	newRec, err := newer.Apply(ctx, scratch, rec)
	switch {
	case errors.As(err, &failure):
		return held(engineHold(failure))
	case err != nil:
		return t, fault.New(fault.Unavailable, "a file could not be de-identified now")
	}
	for _, e := range newRec.Entries {
		if !e.Changed || was[e.Path] == e.Action || rerunRecord[e.Tag] && !strings.Contains(e.Path, "/") {
			continue
		}
		switch {
		case catalogElements[e.Tag] || e.Tag>>16 == 0x7FE0 || e.Tag>>16 == 0x0002:
			details := map[string]string{"cause": TightenCatalogElement}
			if tenant.ValidElementPath(e.Path) {
				details["path"] = e.Path
			}
			return held(tenant.QuarantineDeidFailed, details)
		case !tighteningActions[e.Action]:
			return held(tenant.QuarantineDeidFailed, map[string]string{"cause": TightenNeedsOriginal, "action": e.Action})
		}
		t.Changes = append(t.Changes, Tightening{Path: e.Path, Tag: e.Tag, Action: e.Action})
	}
	if len(t.Changes) == 0 {
		return t, nil
	}
	for _, c := range t.Changes {
		parent, at, ok := locate(ds, c.Path)
		if !ok {
			continue
		}
		parent.Remove(at)
		if sp, sat, sok := locate(scratch, c.Path); sok {
			if elem, has := sp.Get(sat); has {
				if err := parent.Add(elem); err != nil {
					return t, fault.Wrap(fault.Internal, err, "a tightened element cannot be set")
				}
			}
		}
	}
	for _, mt := range []tag.Tag{tag.New(0x0012, 0x0062), tag.New(0x0012, 0x0063), tag.New(0x0012, 0x0064)} {
		if elem, has := scratch.Get(mt); has {
			if err := ds.Add(elem); err != nil {
				return t, fault.Wrap(fault.Internal, err, "the method record cannot be set")
			}
		}
	}
	for _, c := range t.Changes {
		switch c.Tag {
		case 0x0008103E, 0x00080060, 0x00200011:
			if !strings.Contains(c.Path, "/") {
				t.Catalog = append(t.Catalog, CatalogValue{Tag: c.Tag, Value: dsUID(ds, tag.Tag(c.Tag))})
			}
		}
	}
	needles := tightenNeedles(newRec.Needles, t.Changes)
	written := int64(len(file)) + trialWriteHeadroom
	if o.Memory != nil {
		release, err := o.Memory.Reserve(ctx, trialBytes(len(file), 0, check.Reservation(ctx, written, largestText(ds), needles, h.CharacterSet())))
		if reason, details, isHold := checkHold(err); isHold {
			return held(reason, details)
		} else if err != nil {
			return t, fault.New(fault.Unavailable, "a file could not be tightened now")
		}
		defer release()
	}
	mem := &memFile{b: make([]byte, 0, written)}
	if err := writeDataset(mem, ds, h.TransferSyntaxUID, source, littleEndian); err != nil {
		return held(tenant.QuarantineDeidFailed, map[string]string{"cause": "unwritable"})
	}
	out := mem.b
	bound := 2*int64(len(file)) + o.HeaderLimit
	if _, err := o.Check(bound).CheckWritten(ctx, bytes.NewReader(out), int64(len(out)), needles, newRec.Written, h.CharacterSet()); err != nil {
		if reason, details, isHold := checkHold(err); isHold {
			return held(reason, details)
		}
		return t, fault.New(fault.Unavailable, "a tightened file could not be checked now")
	}
	if _, err := header.ReadWithin(bytes.NewReader(out), int64(len(out)), int64(len(out))+o.HeaderLimit); err != nil {
		return held(tenant.QuarantineDeidFailed, map[string]string{"cause": "after_unreadable"})
	}
	t.Written = out
	t.Unnamed, t.UnnamedOver = unnamedOf(newRec)
	return t, nil
}

// tightenNeedles keeps of the newer version's needles those of the elements
// it tightens, and its text lists: the other values the run removed or
// replaced are the archived file's own, kept by the older version's decision.
func tightenNeedles(all engine.Needles, changes []Tightening) engine.Needles {
	var out engine.Needles
	for _, n := range all {
		keep := n.Kind == engine.NeedleList
		for _, c := range changes {
			if n.Path == c.Path || strings.HasPrefix(n.Path, c.Path+"/") || strings.HasPrefix(n.Path, c.Path+"[") {
				keep = true
				break
			}
		}
		if keep {
			out = append(out, n)
		}
	}
	return out
}

// locate returns the data set that holds the element at path (an
// engine.Entry.Path: eight hexadecimal digits per element, "[n]" after a
// sequence's for its item, "/" between levels) and the element's tag; false
// when a level is absent.
func locate(ds *dataset.Dataset, path string) (*dataset.Dataset, tag.Tag, bool) {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		item := -1
		if j := strings.IndexByte(seg, '['); j >= 0 {
			n, err := strconv.Atoi(strings.TrimSuffix(seg[j+1:], "]"))
			if err != nil {
				return nil, 0, false
			}
			item, seg = n, seg[:j]
		}
		v, err := strconv.ParseUint(seg, 16, 32)
		if err != nil {
			return nil, 0, false
		}
		at := tag.Tag(uint32(v))
		if i == len(segs)-1 {
			return ds, at, true
		}
		elem, ok := ds.Get(at)
		if !ok {
			return nil, 0, false
		}
		seq, ok := elem.GetValue().(*sequence.Sequence)
		if !ok || item < 0 {
			return nil, 0, false
		}
		items := seq.Items()
		if item >= len(items) {
			return nil, 0, false
		}
		next, ok := items[item].(*dataset.Dataset)
		if !ok {
			return nil, 0, false
		}
		ds = next
	}
	return nil, 0, false
}
