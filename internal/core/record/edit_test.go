package record

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// editRec is an edit of one scan's two instances, prepared.
func editRec() Correction {
	c := deleteRec()
	c.Kind = KindEdit
	c.Edits = []Replacement{
		{SOPInstanceUID: "1.2.4.1", SeriesUID: "1.2.4", OldRevision: 1, NewRevision: 2, OldSHA256: sumA},
		{SOPInstanceUID: "1.2.4.2", SeriesUID: "1.2.4", OldRevision: 2, NewRevision: 3, OldSHA256: sumB},
	}
	return c
}

// withNewSums gives an edit's instances their new checksums, as its
// publish does.
func withNewSums(c Correction) Correction {
	c.Edits = append([]Replacement(nil), c.Edits...)
	for i := range c.Edits {
		c.Edits[i].NewSHA256 = sumB
	}
	return c
}

// TestEditRecordRoundTrip guards an edit's record in each state it may be
// written in: prepared without its new checksums, published and undone
// with them, read back equal.
func TestEditRecordRoundTrip(t *testing.T) {
	for name, c := range map[string]Correction{
		"prepared":  editRec(),
		"committed": published(withNewSums(editRec()), StateCommitted),
		"undone":    published(withNewSums(editRec()), StateUndone),
		"purging": func() Correction {
			c := published(withNewSums(editRec()), StatePurging)
			c.Purge = &Purge{Removed: 2}
			return c
		}(),
	} {
		b, err := EncodeCorrection(c, tenantA, testKeys)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := DecodeCorrection(bytes.NewReader(b), tenantA, testKeys)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got.KeyVersion, got.MAC = 0, ""
		if !reflect.DeepEqual(got, c) {
			t.Errorf("%s: read back %+v, want %+v", name, got, c)
		}
	}
}

// TestEditRecordRefused guards the rules of an edit's record: each one
// broken is refused as not in the form.
func TestEditRecordRefused(t *testing.T) {
	mut := func(f func(*Correction)) Correction {
		c := editRec()
		c.Edits = append([]Replacement(nil), c.Edits...)
		f(&c)
		return c
	}
	for name, c := range map[string]Correction{
		"no edits":         mut(func(c *Correction) { c.Edits = nil }),
		"no level":         mut(func(c *Correction) { c.Level = "" }),
		"out of order":     mut(func(c *Correction) { c.Edits[0], c.Edits[1] = c.Edits[1], c.Edits[0] }),
		"twice":            mut(func(c *Correction) { c.Edits[1].SOPInstanceUID = c.Edits[0].SOPInstanceUID }),
		"other scan":       mut(func(c *Correction) { c.Edits[0].SeriesUID = "1.9" }),
		"same revision":    mut(func(c *Correction) { c.Edits[0].NewRevision = 1 }),
		"earlier revision": mut(func(c *Correction) { c.Edits[1].NewRevision = 1 }),
		"bad sum":          mut(func(c *Correction) { c.Edits[0].OldSHA256 = "zz" }),
		"published, no new sum": func() Correction {
			return published(editRec(), StateCommitted)
		}(),
		"with a replace": mut(func(c *Correction) { c.Replace = &Replacement{SOPInstanceUID: "1.2.4.1"} }),
		"replace with a series": func() Correction {
			c := deleteRec()
			c.Kind, c.Level = KindReplace, ""
			c.Replace = &Replacement{SOPInstanceUID: "1.2.5.1", SeriesUID: "1.2.4", OldRevision: 1, NewRevision: 2, OldSHA256: sumA, NewSHA256: sumB}
			return c
		}(),
		"undone relabel": func() Correction {
			c := deleteRec()
			c.Kind, c.Level, c.Relabel = KindRelabel, "", &Relabel{Level: LevelSubject, From: "SUB-001", To: "SUB-002"}
			return published(c, StateUndone)
		}(),
	} {
		if err := c.Validate(); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: Validate = %v, want ErrFormat", name, err)
		}
	}
	if !StateCommitted.CanFollow(StateUndone, KindEdit) {
		t.Error("a committed edit cannot be undone")
	}
}

// TestEditRecordSkipsANumberedRevision guards a later revision that is not
// the next: an edit made after an undone one numbers its revisions above
// the undone edit's, which are never numbered again, and its record (and a
// replace's) is in the form.
func TestEditRecordSkipsANumberedRevision(t *testing.T) {
	c := editRec()
	c.Edits = append([]Replacement(nil), c.Edits...)
	c.Edits[0].NewRevision = 3
	if err := c.Validate(); err != nil {
		t.Fatalf("an edit to revision 3 of revision 1 is refused: %v", err)
	}
	r := deleteRec()
	r.Kind, r.Level = KindReplace, ""
	r.Replace = &Replacement{SOPInstanceUID: "1.2.5.1", OldRevision: 1, NewRevision: 4, OldSHA256: sumA, NewSHA256: sumB}
	if err := r.Validate(); err != nil {
		t.Fatalf("a replace to revision 4 of revision 1 is refused: %v", err)
	}
}
