package deidjob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The actions after which an element holds nothing of its value: removed,
// emptied, or removed or emptied unless its IOD requires it (where the
// engine then writes the IOD's dummy).
var emptyingActions = []string{"remove", "empty", "remove_unless_required"}

// findings collects what a check found in one file: the findings, each
// once and in the order found, and the elements it could not judge.
type findings struct {
	v           *verdict
	found       []string
	undecidable int
}

// add records a finding of kind, at the element of tag where when given.
func (f *findings) add(kind, where string) {
	if !slices.Contains(f.found, kind) {
		f.found = append(f.found, kind)
		f.v.tallies[tallyKey(tallyCheck, kind)]++
	}
	if where != "" && len(f.v.tallies) < maxTalliesPerFile {
		f.v.tallies[tallyKey(tallyCheck, kind, where)]++
	}
}

// note records an element the check cannot judge from the archive.
func (f *findings) note(where string) {
	if f.undecidable == 0 {
		f.v.tallies[tallyKey(tallyCheck, tallyUndecidable)]++
	}
	f.undecidable++
	if len(f.v.tallies) < maxTalliesPerFile {
		f.v.tallies[tallyKey(tallyCheck, tallyUndecidable, where)]++
	}
}

// verdict sets the file's verdict: refused with its first finding, a note
// when only some elements could not be judged, passed otherwise.
func (f *findings) verdict() {
	switch {
	case len(f.found) > 0:
		f.v.kind, f.v.code, f.v.detail = resultRefused, tenant.FailureCheckFailed, f.found[0]
	case f.undecidable > 0:
		f.v.kind, f.v.code, f.v.detail = resultNoted, tenant.FailureUndecided, detailUndecidable
	case f.v.kind == 0:
		f.v.kind = resultPassed
	}
}

// check verifies the file against the version its instance records.
//
// It reads the file twice: once as archived, and once for the engine to
// explain under the version (engine.Explain applies the version to the
// data set it is given), so what the version leaves of each element is
// compared with what the archive holds. An element the version removes,
// or empties, that the archive holds with a value is a finding; one the
// version removes or empties unless the IOD requires it is a finding when
// it resolves to removal or emptiness, or to a dummy the archive does not
// hold, and none when the IOD keeps it as it is. An element whose decision
// rests on a condition the archive cannot answer (receptionFacts,
// judgeable) is a note, never a finding and never a silent pass.
func (j *judge) check(ctx context.Context, in tenant.ScanInstance, file []byte, h *header.Header, herr error, facts receptionFacts, v *verdict) error {
	if in.Profile.IsZero() {
		v.kind, v.code, v.detail = resultNoted, tenant.FailureNotDeidentified, detailAsReceived
		v.tallies[tallyAsReceived]++
		return nil
	}
	fs := &findings{v: v}
	defer fs.verdict()
	if herr != nil {
		fs.add(findingUnreadable, "")
		return nil
	}
	archived, _, _, err := h.Dataset(bytes.NewReader(file))
	if err != nil {
		fs.add(findingUnreadable, "")
		return nil
	}
	ds, _, _, err := h.Dataset(bytes.NewReader(file))
	if err != nil {
		fs.add(findingUnreadable, "")
		return nil
	}
	if text, _ := textOf(archived, tagIdentityRemoved); text != identityRemovedYES {
		fs.add(findingIdentity, "00120062")
	}
	if text, _ := textOf(archived, tagDeidMethod); !slices.Contains(strings.Split(text, `\`), in.Profile.String()) {
		fs.add(findingMethod, "00120063")
	}
	uids := map[string]string{}
	if collectUIDs(archived, uids) {
		v.tallies[tallyUIDsNotChecked]++
	}
	if err := j.receivedUIDs(ctx, uids, fs); err != nil {
		return err
	}
	en, err := j.engineFor(ctx, in.Profile)
	if err != nil {
		return err
	}
	if en == nil {
		fs.add(findingEngine, "")
		return nil
	}
	rec := facts.rec
	rec.CharacterSet = h.CharacterSet()
	explained, err := en.Explain(ctx, ds, rec)
	var failure *engine.Failure
	switch {
	case errors.As(err, &failure):
		fs.add(findingEngine, "")
		return nil
	case err != nil:
		return fault.New(fault.Unavailable, "a file could not be explained now")
	}
	keptTop := map[uint32]bool{}
	for _, x := range explained {
		if e := x.Entry; e.Action == "keep" && !strings.Contains(e.Path, "/") {
			keptTop[e.Tag] = true
		}
	}
	rules := j.rules[in.Profile]
	unknown := func(rule int) bool {
		if rule < 0 || rule >= len(rules) {
			return false
		}
		for _, c := range rules[rule].GetRule().GetWhen() {
			if !judgeable(c.GetSubject(), facts, keptTop) {
				return true
			}
		}
		return false
	}
	for _, x := range explained {
		e := x.Entry
		before, held := elemAt(archived, e.Path)
		if !held {
			continue // made by the version, or by a set rule: not the archive's
		}
		where := fmt.Sprintf("%08X", e.Tag)
		decidedUnknown := unknown(e.Rule)
		otherUnknown := false
		for _, o := range x.Others {
			if o.WhyNot == engine.WhyConditions && unknown(o.Rule) {
				otherUnknown = true
			}
		}
		kind := ""
		if slices.Contains(emptyingActions, e.Action) {
			after, present := elemAt(ds, e.Path)
			switch {
			case !present && (e.Tag>>16)%2 == 1:
				kind = findingPrivate
			case !present:
				kind = findingRemoved
			case after == "" && before != "":
				kind = findingKeptValue
			case e.Action == "remove_unless_required" && after != before:
				kind = findingKeptValue
			}
		}
		switch {
		case kind != "" && (decidedUnknown || otherUnknown):
			fs.note(where)
		case kind != "":
			fs.add(kind, where)
		case decidedUnknown && !slices.Contains(emptyingActions, e.Action):
			// A value kept by a rule whose condition the archive cannot
			// answer: had the file said otherwise, the version would
			// have removed it.
			fs.note(where)
		}
	}
	return nil
}

// elemAt returns what the data set holds at path (a record's path, such as
// "00081115[0]/0020000E"), and whether it holds the element: a value's
// bytes without their padding, or a sequence's number of items.
func elemAt(ds *dataset.Dataset, path string) (string, bool) {
	steps := strings.Split(path, "/")
	cur := ds
	for i, s := range steps {
		ts, index, item := strings.Cut(s, "[")
		t, err := strconv.ParseUint(ts, 16, 32)
		if err != nil || len(ts) != 8 || cur == nil {
			return "", false
		}
		elem, ok := cur.Get(tag.Tag(t))
		if !ok {
			return "", false
		}
		seq, isSeq := elem.GetValue().(*sequence.Sequence)
		if i == len(steps)-1 {
			if item {
				return "", false
			}
			if isSeq {
				return "items:" + strconv.Itoa(seq.Length()), true
			}
			b, _ := elem.Bytes()
			return strings.TrimRight(string(b), "\x00 "), true
		}
		n, err := strconv.Atoi(strings.TrimSuffix(index, "]"))
		if !item || err != nil || !isSeq || n < 0 || n >= seq.Length() {
			return "", false
		}
		it, err := seq.Get(n)
		if err != nil {
			return "", false
		}
		cur, _ = it.(*dataset.Dataset)
	}
	return "", false
}

// receivedUIDs looks up the file's UIDs outside the registry: remapped
// under the tenant's UID key among every UID the catalog holds, and as
// they are among what was archived as received. One found is a UID some
// received file carried, unless the UID is itself one the catalog holds
// for a de-identified instance: then the remapping made it, and a hit is
// that instance's data received again, not an original.
func (j *judge) receivedUIDs(ctx context.Context, uids map[string]string, fs *findings) error {
	if len(uids) == 0 {
		return nil
	}
	plain := make([]string, 0, len(uids))
	keyed := make([]string, 0, len(uids))
	byKeyed := make(map[string]string, len(uids))
	for u := range uids {
		plain = append(plain, u)
		r := engine.RemapUID(j.uidKey, u)
		keyed = append(keyed, r)
		byKeyed[r] = u
	}
	seen, err := j.p.SeenUIDs(ctx, keyed, plain)
	if err != nil {
		return err
	}
	for _, r := range seen.Keyed {
		if u := byKeyed[r]; !slices.Contains(seen.Deidentified, u) {
			fs.add(findingReceivedUID, uids[u])
		}
	}
	for _, u := range seen.AsReceived {
		fs.add(findingReceivedUID, uids[u])
	}
	return nil
}

// collectUIDs adds to out every UID value of ds outside the registry, at
// any depth, with the tag it was found under, at most tenant.MaxSeenUIDs;
// it reports whether there were more.
func collectUIDs(ds *dataset.Dataset, out map[string]string) bool {
	past := false
	for _, elem := range ds.ElementsInTagOrder() {
		if seq, ok := elem.GetValue().(*sequence.Sequence); ok {
			for i := range seq.Length() {
				item, err := seq.Get(i)
				if err != nil {
					continue
				}
				if child, ok := item.(*dataset.Dataset); ok && collectUIDs(child, out) {
					past = true
				}
			}
			continue
		}
		if string(elem.GetVR()) != "UI" {
			continue
		}
		b, ok := elem.Bytes()
		if !ok {
			continue
		}
		for _, u := range strings.Split(strings.TrimRight(string(b), "\x00 "), `\`) {
			u = strings.TrimSpace(u)
			if u == "" || engine.Registered(u) || !validUID(u) {
				continue
			}
			if _, seen := out[u]; seen {
				continue
			}
			if len(out) >= tenant.MaxSeenUIDs {
				past = true
				break
			}
			t, _ := elem.Tag()
			out[u] = fmt.Sprintf("%08X", uint32(t))
		}
	}
	return past
}
