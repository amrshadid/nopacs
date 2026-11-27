package correct

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// EditPreviewRequest asks what an edit would change.
type EditPreviewRequest struct {
	// Target and Changes are the edit's.
	Target  tenant.CorrectionTarget
	Changes []EditChange
	// ShowValues answers each attribute's value now. The caller refuses it
	// to a key that may see only archived values in their de-identified
	// form, and records that it was asked; the values are those the
	// archive holds, read from each scan's metadata.
	ShowValues bool
}

// EditPreview is what an edit would change, and the token it must carry.
type EditPreview struct {
	// Target and Changes are what was asked.
	Target  tenant.CorrectionTarget
	Changes []EditChange
	// Instances are every instance it changes, in the order of
	// tenant.Edits.EditTargets.
	Instances []EditPreviewInstance
	// Storages are the storages that hold them, by name.
	Storages []string
	// Issued is when the preview was made; Confirm its token.
	Issued  time.Time
	Confirm string
}

// EditPreviewInstance is one instance an edit changes.
type EditPreviewInstance struct {
	// Path is its scan's labels; UID its SOP Instance UID; Revision its
	// revision now, the edit giving it the next.
	Path     label.Path
	UID      string
	Revision int
	// Attributes are what each change does to it, in the changes' order.
	Attributes []EditPreviewAttribute
}

// EditPreviewAttribute is what one change does to one instance.
type EditPreviewAttribute struct {
	// Tag is the attribute's.
	Tag uint32
	// Present says the instance holds it now, as its metadata says; Known
	// is false when the metadata omits the instance's data set.
	Present, Known bool
	// Old is its value now, only when ShowValues was asked.
	Old string
	// New is the value the change gives it; Remove says it is removed.
	New    string
	Remove bool
}

// PreviewEdit answers what an edit of r.Target would change, as the catalog
// holds it now, and its token: every instance and attribute, its old value
// hidden unless ShowValues. A change no edit may make, or a target of more
// than tenant.MaxEditInstances instances, is refused.
func (e *Engine) PreviewEdit(ctx context.Context, r EditPreviewRequest) (EditPreview, error) {
	p, _, err := e.editPreview(ctx, r.Target, r.Changes, e.now(), r.ShowValues)
	if err != nil {
		return EditPreview{}, err
	}
	if e.cfg.ConfirmKey != nil {
		mac, err := e.cfg.ConfirmKey.MAC([]byte(p.Confirm))
		if err != nil {
			return EditPreview{}, err
		}
		p.Confirm = tenant.KeyedConfirm(p.Confirm, mac)
	}
	return p, nil
}

// edits returns the tenant's database as it keeps edits.
func (e *Engine) edits() (tenant.Edits, error) {
	ed, ok := e.cfg.Store.(tenant.Edits)
	if !ok {
		return nil, fault.New(fault.Unavailable, "the tenant's database keeps no edits")
	}
	return ed, nil
}

// editTargets returns the instances an edit of target changes, refusing a
// target of more than tenant.MaxEditInstances.
func (e *Engine) editTargets(ctx context.Context, target tenant.CorrectionTarget) ([]tenant.EditTarget, error) {
	ed, err := e.edits()
	if err != nil {
		return nil, err
	}
	targets, err := ed.EditTargets(ctx, target, tenant.MaxEditInstances+1)
	if err != nil {
		return nil, err
	}
	if len(targets) > tenant.MaxEditInstances {
		return nil, fault.New(fault.Precondition, "the target holds more than "+strconv.Itoa(tenant.MaxEditInstances)+
			" instances, the most one edit changes; edit it a scan at a time")
	}
	return targets, nil
}

// editPreview makes the preview of an edit at the time issued, with its
// store token (editConfirm), and the instances it read.
func (e *Engine) editPreview(ctx context.Context, target tenant.CorrectionTarget, changes []EditChange, issued time.Time,
	show bool) (EditPreview, []tenant.EditTarget, error) {
	if err := target.Validate(); err != nil {
		return EditPreview{}, nil, err
	}
	if err := checkChanges(changes); err != nil {
		return EditPreview{}, nil, err
	}
	if err := checkTargetChanges(target, changes); err != nil {
		return EditPreview{}, nil, err
	}
	targets, err := e.editTargets(ctx, target)
	if err != nil {
		return EditPreview{}, nil, err
	}
	p := EditPreview{Target: target, Changes: changes, Issued: issued.UTC().Truncate(time.Microsecond)}
	lines, err := e.scanLines(ctx, targets)
	if err != nil {
		return EditPreview{}, nil, err
	}
	for _, t := range targets {
		if n := len(p.Storages); n == 0 || p.Storages[n-1] != t.Storage {
			p.Storages = append(p.Storages, t.Storage)
		}
		in := EditPreviewInstance{Path: t.Path, UID: t.UID, Revision: t.Revision}
		values, known := datasetValues(lines[t.UID])
		for _, c := range changes {
			a := EditPreviewAttribute{Tag: c.Tag, New: c.Value, Remove: c.Remove, Known: known}
			old, ok := values[tagKey(c.Tag)]
			a.Present = ok
			if show {
				a.Old = old
			}
			in.Attributes = append(in.Attributes, a)
		}
		p.Instances = append(p.Instances, in)
	}
	p.Confirm = editConfirm(p, targets)
	return p, targets, nil
}

// editConfirm returns the store's token of an edit's preview: its issue
// time, then a digest of the target, the changes and each instance with
// its revision and archived checksum, so an edit carrying it acts on what
// was shown, and one whose target changed since does not match. The form
// is a delete's (tenant.ConfirmIssued reads its time); the digest is of
// its own text, so a token of one never matches the other.
func editConfirm(p EditPreview, targets []tenant.EditTarget) string {
	var b strings.Builder
	w := func(parts ...string) {
		for _, s := range parts {
			b.WriteString(strconv.Itoa(len(s)))
			b.WriteByte(':')
			b.WriteString(s)
		}
	}
	issued := strconv.FormatInt(p.Issued.UnixMicro(), 10)
	t := p.Target
	w("nopacs-edit-preview-1", issued, t.Subject, t.Session, t.Scan, t.Instance, strconv.Itoa(len(p.Changes)))
	for _, c := range p.Changes {
		w(strconv.FormatUint(uint64(c.Tag), 16), strconv.FormatBool(c.Remove), c.Value)
	}
	w(strconv.Itoa(len(targets)))
	for _, x := range targets {
		w(x.Storage, x.Path.Subject, x.Path.Session, x.Path.Scan, x.UID, strconv.Itoa(x.Revision), x.SHA256.String())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "c1." + issued + "." + hex.EncodeToString(sum[:])
}

// unkeyEditConfirm checks the token an edit carries against a preview of
// the same edit made again at the token's issue time, and returns the
// store's token and the instances that preview read, which the edit then
// changes. Without a confirm key the token is the store's own.
func (e *Engine) unkeyEditConfirm(ctx context.Context, target tenant.CorrectionTarget, changes []EditChange,
	token string) (string, []tenant.EditTarget, error) {
	var issued time.Time
	var mac []byte
	if e.cfg.ConfirmKey != nil {
		var ok bool
		if issued, mac, ok = tenant.SplitKeyedConfirm(token); !ok {
			return "", nil, fault.New(fault.Precondition, "the confirm token is not one this server's preview gave")
		}
	} else {
		var ok bool
		if issued, ok = tenant.ConfirmIssued(token); !ok {
			return "", nil, fault.New(fault.Precondition, "the confirm token is not one an edit's preview gave")
		}
	}
	p, targets, err := e.editPreview(ctx, target, changes, issued, false)
	if err != nil {
		return "", nil, err
	}
	if e.cfg.ConfirmKey == nil {
		if p.Confirm != token {
			return "", nil, errTokenMismatch
		}
		return token, targets, nil
	}
	want, err := e.cfg.ConfirmKey.MAC([]byte(p.Confirm))
	if err != nil {
		return "", nil, err
	}
	if !hmac.Equal(mac, want) {
		return "", nil, errTokenMismatch
	}
	return p.Confirm, targets, nil
}

// editTokenKey is what an edit's token is remembered by: the token and the
// changes it was given with (the token's digest covers them), so the same
// token with other changes is never answered with the edit it made.
func editTokenKey(token string, changes []EditChange) string {
	var b strings.Builder
	b.WriteString("edit\x00")
	b.WriteString(token)
	for _, c := range changes {
		b.WriteString("\x00" + strconv.FormatUint(uint64(c.Tag), 16) + "\x00" + strconv.FormatBool(c.Remove) + "\x00" + c.Value)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// rememberEdit keeps, for an edit's token and changes, the correction they
// made and its target: the edit gave its target new revisions, so a
// preview made again at the token's time no longer matches it, and a lost
// answer asked again is answered with the edit itself.
func (e *Engine) rememberEdit(key, id string, target tenant.CorrectionTarget) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.usedTokens) >= maxUsedTokens {
		clear(e.usedTokens)
	}
	e.usedTokens[key] = usedToken{target: target, id: id}
}

// usedEdit returns the correction and target rememberEdit kept for key.
func (e *Engine) usedEdit(key string) (string, tenant.CorrectionTarget, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	u, ok := e.usedTokens[key]
	return u.id, u.target, ok && u.id != ""
}

// scanLines reads, from each scan's metadata on its storage, the line of
// every target, as JSON, by SOP Instance UID.
func (e *Engine) scanLines(ctx context.Context, targets []tenant.EditTarget) (map[string][]byte, error) {
	want := make(map[string]bool, len(targets))
	type scanAt struct {
		storage string
		path    label.Path
	}
	var scans []scanAt
	for _, t := range targets {
		want[t.UID] = true
		if n := len(scans); n == 0 || scans[n-1] != (scanAt{t.Storage, t.Path}) {
			scans = append(scans, scanAt{t.Storage, t.Path})
		}
	}
	out := make(map[string][]byte, len(targets))
	for _, s := range scans {
		st, err := e.cfg.OpenStorage(ctx, s.storage)
		if err != nil {
			return nil, err
		}
		if err := readLines(ctx, st, s.path, func(in sidecar.Instance) error {
			if !want[in.SOPInstanceUID] {
				return nil
			}
			b, err := json.Marshal(in)
			if err != nil {
				return fault.Wrap(fault.Internal, err, "an instance's line cannot be kept")
			}
			out[in.SOPInstanceUID] = b
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for uid := range want {
		if out[uid] == nil {
			return nil, fault.New(fault.Precondition, "a scan's metadata holds no line of one of the target's instances")
		}
	}
	return out, nil
}

// readLines gives each line of a scan's metadata to fn, in order.
func readLines(ctx context.Context, st blob.Store, p label.Path, fn func(sidecar.Instance) error) error {
	sk, _ := layout.MetadataKey(p)
	rc, err := st.Read(ctx, sk, 0, -1)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	rd := sidecar.NewReader(rc)
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fault.Wrap(fault.Unavailable, err, "the scan's metadata cannot be read")
		}
		if err := fn(in); err != nil {
			return err
		}
	}
}

// tagKey writes a tag as the DICOM JSON model names it (PS3.18 F.2.1.1):
// eight upper-case hexadecimal digits.
func tagKey(t uint32) string {
	const digits = "0123456789ABCDEF"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = digits[t&0xf]
		t >>= 4
	}
	return string(b)
}

// datasetValues reads the top-level values of a line's data set (DICOM
// JSON, PS3.18 F.2), each attribute's values joined by a backslash and a
// person name by its alphabetic form; false when the line omits its data
// set.
func datasetValues(line []byte) (map[string]string, bool) {
	var in sidecar.Instance
	if json.Unmarshal(line, &in) != nil || in.Dataset == nil {
		return nil, false
	}
	var ds map[string]struct {
		Value []json.RawMessage `json:"Value"`
	}
	if json.Unmarshal(in.Dataset, &ds) != nil {
		return nil, false
	}
	out := make(map[string]string, len(ds))
	for k, a := range ds {
		var parts []string
		for _, v := range a.Value {
			var s string
			var pn struct {
				Alphabetic string `json:"Alphabetic"`
			}
			switch {
			case json.Unmarshal(v, &s) == nil:
			case json.Unmarshal(v, &pn) == nil:
				s = pn.Alphabetic
			default:
				s = strings.Trim(string(v), `"`)
			}
			parts = append(parts, s)
		}
		out[k] = strings.Join(parts, `\`)
	}
	return out, true
}
