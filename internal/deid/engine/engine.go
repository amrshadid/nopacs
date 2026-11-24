package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/amrshadid/go-dicom/anonymize"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/valuerep"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// Bounds of a data set the engine processes. A file past one is refused
// whole, never processed in part.
const (
	// MaxDepth is how deep sequences may nest (CauseTooDeep).
	MaxDepth = 64
	// MaxElements is how many elements a data set may hold, sequences'
	// items included (CauseTooLarge).
	MaxElements = 1 << 20
	// MaxValueLength is the longest value an action reads (CauseBadValue):
	// keep, remove and empty read no value and take any.
	MaxValueLength = 1 << 20
	// MinKeyLength is the shortest key the engine takes: the output size
	// of SHA-256, the least RFC 2104 section 3 recommends for HMAC, below
	// which an identifier could be found from its code by trying them.
	MinKeyLength = 32
	// MaxLookups is the most values one file has lookup tables assign
	// codes to (CauseTooLarge past it): the most a tenant's tables assign
	// in one call (tenant.MaxLookupBatch), so a file's codes are one call
	// and all or none.
	MaxLookups = 1000
	// maxMethodLength is the bound of De-identification Method (0012,0063),
	// an LO value (PS3.5 6.2).
	maxMethodLength = 64
)

// Keys are a tenant's secrets the actions are keyed with, as bytes; the
// engine keeps its own copies.
type Keys struct {
	// UID keys UID remapping (RemapUID).
	UID []byte
	// Date keys the subjects' date shifts (ShiftDays).
	Date []byte
	// Hash keys the hash and lookup actions and templates' placeholders:
	// the tenant's naming key, so a hashed Patient ID equals its subject's
	// label code.
	Hash []byte
}

// Reception is what a file's reception says of it, which conditions read.
// An empty field is absent.
type Reception struct {
	// Receiver is the receiver's name.
	Receiver string
	// CallingAETitle is the sender's AE title (PS3.8 9.3.2).
	CallingAETitle string
	// CalledAETitle is the AE title the sender called.
	CalledAETitle string
	// CharacterSet is the file's Specific Character Set (0008,0005) as
	// received, its values joined by a backslash, "" when the file
	// declared none; nil when the caller does not say. The DICOM library
	// decodes a file's text into UTF-8 and declares ISO_IR 192 over it, so
	// the data set no longer says what the file was written in; but a
	// sequence held as UN bytes, and a private value of VR UN, are bytes
	// as the file wrote them, which the engine decodes itself with this
	// (PS3.5 6.1.2.3). With nil, such text is taken only when it is in the
	// default repertoire, and a file with any other is refused where the
	// text would be kept.
	CharacterSet *string
	// Subject, when set, identifies the file's subject for its date shift
	// (ShiftDays) in place of its Patient ID (0010,0020). It is for a job
	// over an archive, whose files no longer hold the Patient ID the
	// profile emptied: the job gives the instance's subject label from the
	// catalog, used only as the shift's key and never written into the
	// file or its record. A reception never sets it: a file received
	// without a Patient ID has no subject, and a date to shift refuses it
	// (CauseNoSubject).
	Subject string
}

// Lookups reaches a tenant's lookup tables. A table is keyed by the keyed
// hash of a value (the naming rule's {#...} code of it), never the value.
type Lookups interface {
	// Code returns the code table holds for key, and whether it holds one.
	Code(ctx context.Context, table, key string) (code string, found bool, err error)
	// Assign returns the codes the tables hold for keys, in their order,
	// first giving each key its table's next code when it holds none, all
	// at once or none: the engine asks once per file, for every value the
	// file's plan assigns, so a file takes all its codes or none.
	Assign(ctx context.Context, keys []LookupKey) ([]string, error)
}

// LookupKey is a value a lookup table is asked for: the table, and the
// keyed hash of the value.
type LookupKey struct {
	// Table is the table's name.
	Table string
	// Key is the keyed hash of the value.
	Key string
}

// Options are what an Engine takes besides its profile and keys.
type Options struct {
	// Lookups reaches the lookup tables; nil when the profile has no
	// lookup action, and Apply fails for one that does.
	Lookups Lookups
	// Types gives attributes' Types in their IODs, for the action that
	// removes an element unless its IOD requires it; nil for the DICOM
	// library's IOD tables (IODTypes).
	Types Types
	// Method, when set, is written into De-identification Method
	// (0012,0063) after the meanings of the Annex E codes: the profile and
	// version that made the file, as the caller names them. At most 64 characters of the default repertoire.
	Method string
}

// errNoDataset is Apply or Explain given no data set.
var errNoDataset = errors.New("engine: no data set")

// Engine applies one expanded profile version under one tenant's keys. It
// does no I/O but through Lookups, reads no clock and no random source,
// and is never changed after New, so one Engine serves every file at once
// and the same file always comes out the same.
type Engine struct {
	dec     Decider
	keys    Keys
	lookups Lookups
	types   Types
	method  string
	// codes are the PS3.16 CID 7050 codes of the Annex E bases the
	// profile is built on, recorded in every file (planMethod).
	codes []anonymize.MethodCode
	// safeVRs is set when the profile is built on the Retain Safe Private
	// base of the edition of the library's Table E.3.10-1, whose VRs a
	// kept private element held as UN then takes (keepSafePrivate).
	safeVRs bool
	// temporal is what Longitudinal Temporal Information Modified
	// (0028,0303) says by the profile's dates option, empty for none.
	temporal string
	// received are the top-level elements templates read, as received.
	received []uint32
	// valueCost is the compiled size of the value patterns together, zero
	// when the profile has none; a value pattern then reads the value of
	// each text element (Location.Value), each byte at that cost.
	valueCost int64
	// pixels decides the pixel rules; nil for an Engine over a Decider
	// of a test, which matches none.
	pixels pixelRules
	seams  engineSeams
}

// engineSeams are the points a test of this package reaches into an
// Engine through; nil in every Engine New makes.
type engineSeams struct {
	// beforeOp is called before apply makes the change of index i.
	beforeOp func(i int)
}

// New compiles an expanded profile, as profile.Expand made it or a stored
// version holds it, for Apply. It refuses keys shorter than MinKeyLength
// or equal to one another, and a method that is not an LO value of the
// default repertoire.
func New(e *v1.ExpandedProfile, keys Keys, opts Options) (*Engine, error) {
	dec, err := newProgramDecider(e)
	if err != nil {
		return nil, err
	}
	bases := e.GetBases()
	for _, b := range bases {
		// A stored version runs only with the Annex E tables of the
		// edition its bases name: the safe private attributes' VRs are
		// read from the library's table as the file is processed, so a
		// version of another edition would mean something else here.
		if _, edition, ok := profile.AnnexEName(b); ok && edition != profile.AnnexEEdition {
			return nil, fmt.Errorf("engine: the profile is built on %s, and this build runs the Annex E bases of edition %s only", b, profile.AnnexEEdition)
		}
	}
	en, err := newEngine(dec, keys, opts)
	if err != nil {
		return nil, err
	}
	en.pixels = programPixels{dec.prog}
	weakened, err := profile.KeepsBaseChange(e)
	if err != nil {
		return nil, err
	}
	if !weakened {
		en.codes = profile.MethodCodes(bases)
	}
	en.safeVRs = slices.Contains(bases, profile.AnnexE("retain-safe-private")) && anonymize.SafePrivateEdition == profile.AnnexEEdition
	// PS3.15 E.3.6: under the Retain Longitudinal Temporal Information
	// with Full Dates option, Longitudinal Temporal Information Modified
	// (0028,0303) "shall be added ... with a Value of UNMODIFIED"; under
	// the option with Modified Dates, "with a Value of MODIFIED", whether
	// or not a file holds a date. A later base overrides an earlier.
	for _, b := range bases {
		switch b {
		case profile.AnnexE("retain-longitudinal-full-dates"):
			en.temporal = "UNMODIFIED"
		case profile.AnnexE("retain-longitudinal-modified-dates"):
			en.temporal = "MODIFIED"
		}
	}
	return en, nil
}

// newEngine makes an Engine over any Decider.
func newEngine(dec Decider, keys Keys, opts Options) (*Engine, error) {
	for _, k := range []struct {
		name string
		key  []byte
	}{{"uid", keys.UID}, {"date", keys.Date}, {"hash", keys.Hash}} {
		if len(k.key) < MinKeyLength {
			return nil, fmt.Errorf("engine: the %s key is shorter than %d bytes", k.name, MinKeyLength)
		}
	}
	// Each key is its own secret: one given for another would let a code
	// of one kind be checked against the other, and destroying one (when
	// a data set is closed) would not end what it keys.
	if bytes.Equal(keys.UID, keys.Date) || bytes.Equal(keys.UID, keys.Hash) || bytes.Equal(keys.Date, keys.Hash) {
		return nil, errors.New("engine: two of the keys are equal; each must be its own secret")
	}
	if opts.Method != "" {
		if len(opts.Method) > maxMethodLength || !isASCII(opts.Method) || valuerep.ValidateValue("LO", opts.Method) != nil {
			return nil, fmt.Errorf("engine: the method is not an LO value of at most %d characters of the default repertoire", maxMethodLength)
		}
	}
	en := &Engine{
		dec:     dec,
		keys:    Keys{UID: clone(keys.UID), Date: clone(keys.Date), Hash: clone(keys.Hash)},
		lookups: opts.Lookups,
		types:   opts.Types,
		method:  opts.Method,
	}
	if en.types == nil {
		en.types = defaultTypes
	}
	if vr, ok := dec.(valueReader); ok {
		en.valueCost = vr.ValueCost()
	}
	seen := map[uint32]bool{}
	for _, tmpl := range dec.Templates() {
		for _, t := range placeholders(tmpl) {
			if !seen[t] {
				seen[t] = true
				en.received = append(en.received, t)
			}
		}
	}
	return en, nil
}

// defaultTypes is the IOD Types every Engine without its own shares, so
// each answer is worked out once per process.
var defaultTypes = &IODTypes{}

// Apply de-identifies ds in place by the profile, and returns what it did
// to each element. Every element is visited: the top level, every item of
// every sequence, private blocks; each takes the action of the rule the
// profile's precedence chooses, or the default, but that the floor
// (floor.go) never lets one that names or identifies the person stay.
// Then the elements set rules make are added, and the de-identification
// is recorded in the data set (planMethod): Patient Identity Removed
// (0012,0062) YES, De-identification Method Code Sequence (0012,0064) with
// the codes of the Annex E bases the profile is built on, De-identification
// Method (0012,0063) with their meanings and Options.Method, Longitudinal Temporal Information Modified (0028,0303) MODIFIED when a
// date was shifted (PS3.15 E.1.1, E.3.6), and Specific Character Set
// (0008,0005) as the text that stays needs it. The pixels are worked on as
// ApplyImage says, with an Image of no source, no frame reader and no
// bound: for a data set that holds its Pixel Data in memory.
//
// Everything is worked out before the data set is changed. A file the
// profile cannot process returns a *Failure and leaves ds as it was, never
// half processed; so does a fault of the engine itself (a panic, recovered
// as CauseInternal, the changes made so far undone). Any other error (a
// lookup table that does not answer, a canceled ctx) says nothing of the
// file and leaves it as it was too.
func (en *Engine) Apply(ctx context.Context, ds *dataset.Dataset, rec Reception) (out *Record, err error) {
	return en.apply(ctx, ds, rec, Image{}, nil)
}

// ApplyImage is Apply for a data set whose Pixel Data may be left in the
// file, with what the work on its pixels reads (Image). The pixels are
// judged by the header as received: an image at risk of burned-in text
// (anonymize.BurnedInRisk) no pixel rule matches is refused
// (CauseBurnedInRisk); the last pixel rule that matches decides, filling
// rectangles it names (redact) or the detector finds (detect), or leaving
// the pixels (accept); pixels left compressed lose the comment and
// application segments of the JPEG family, and a syntax whose segments
// are not searched is refused as the profile says. A filled image records
// Burned In Annotation (0028,0301) NO and the Clean Pixel Data Option's
// code; a detected one "Burned-in text redacted where detected,
// unverified" in De-identification Method (PS3.15 E.3.1). Record.Pixels
// says what was done. Pixel Data left in the file is filled, and decoded,
// as a writer copies it (dataset.Dataset.DecompressRedactingRegions), a
// frame at a time, so memory does not grow with the image; a frame that
// will not decode is then the writer's error, wrapping
// ErrPixelsUndecodable.
func (en *Engine) ApplyImage(ctx context.Context, ds *dataset.Dataset, rec Reception, img Image) (*Record, error) {
	return en.apply(ctx, ds, rec, img, nil)
}

// apply is ApplyImage, recording into locs, when not nil, the location of
// each element a rule was asked about.
func (en *Engine) apply(ctx context.Context, ds *dataset.Dataset, rec Reception, img Image, locs map[int]profile.Location) (out *Record, err error) {
	if ds == nil {
		return nil, errNoDataset
	}
	r := &run{en: en, ctx: ctx, top: ds, record: &Record{}, source: sourceFrom(rec.CharacterSet),
		rawSources: map[*dataset.Dataset]textSource{}, locs: locs, subject: rec.Subject}
	defer func() {
		if p := recover(); p != nil {
			r.rollback()
			// The panic's value may hold a value of the file; it is not
			// passed on.
			out, err = nil, &Failure{Cause: CauseInternal}
		}
	}()
	r.facts = readFacts(ds, rec)
	r.received = map[uint32]string{}
	for _, t := range en.received {
		if v, ok := topText(ds, t); ok {
			r.received[t] = v
		}
	}
	if err := r.walk(ds, nil, "", scope{charset: charsetOf(ds, charsetAbsent), source: r.source}); err != nil {
		return nil, err
	}
	if err := r.planSets(); err != nil {
		return nil, err
	}
	if err := r.finishCleans(); err != nil {
		return nil, err
	}
	if locs == nil {
		// An explanation judges elements, never pixels.
		if err := r.planPixels(img); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.finishAssigns(); err != nil {
		return nil, err
	}
	if err := r.planMethod(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.apply()
	r.declareCharset()
	r.applyPixels()
	return r.record, nil
}

// clone copies a key.
func clone(b []byte) []byte { return append([]byte(nil), b...) }
