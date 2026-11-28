package record

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// moveRec is a move of scanOf's session to the subject SUB-002.
func moveRec() Correction {
	c := deleteRec()
	c.Kind, c.Level, c.Move = KindMove, "", &Move{Level: LevelSession, From: "SUB-001", To: "SUB-002"}
	return c
}

// TestMoveRecord guards a move's record: a session's move and a subject's
// merge are written and read back equal, undone among their states, and
// each rule of its form refuses what breaks it.
func TestMoveRecord(t *testing.T) {
	merge := moveRec()
	merge.Move.Level, merge.Scans = LevelSubject, []Scan{scanOf(), otherScan(), otherSession()}
	for name, c := range map[string]Correction{
		"session move": moveRec(), "merge": merge, "committed": published(moveRec(), StateCommitted),
		"undone": published(merge, StateUndone),
	} {
		b, err := encode(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		back, err := decodeRec(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		back.KeyVersion, back.MAC = 0, ""
		if !reflect.DeepEqual(back, c) {
			t.Fatalf("%s: read back %+v; wrote %+v", name, back, c)
		}
	}
	mv := func(f func(*Correction)) Correction { c := moveRec(); f(&c); return c }
	for name, c := range map[string]Correction{
		"missing":      mv(func(c *Correction) { c.Move = nil }),
		"level scan":   mv(func(c *Correction) { c.Move.Level = LevelScan }),
		"same subject": mv(func(c *Correction) { c.Move.To = "sub-001" }),
		"bad label":    mv(func(c *Correction) { c.Move.To = "a/b" }),
		"from":         mv(func(c *Correction) { c.Move.From = "SUB-003" }),
		"two sessions": mv(func(c *Correction) { c.Scans = append(c.Scans, otherSession()) }),
		"with level":   mv(func(c *Correction) { c.Level = LevelSession }),
		"with relabel": mv(func(c *Correction) { c.Relabel = &Relabel{Level: LevelSession, From: "20240131", To: "x"} }),
		"delete moves": func() Correction {
			c := deleteRec()
			c.Move = &Move{Level: LevelSession, From: "SUB-001", To: "SUB-002"}
			return c
		}(),
		"with instances": mv(func(c *Correction) { c.Instances = []string{"1.2.5.1"} }),
	} {
		if b, err := encode(c); err == nil || !errors.Is(err, ErrFormat) {
			t.Errorf("%s: wrote %s, %v", name, b, err)
		}
	}
	if !StateCommitted.CanFollow(StateUndone, KindMove) || StateCommitted.CanFollow(StateUndone, KindRelabel) {
		t.Fatal("a move, and not a relabel, is undone after its publish")
	}
	if !KindMove.CopiesObjects() || !KindRelabel.CopiesObjects() || KindEdit.CopiesObjects() || KindDelete.CopiesObjects() {
		t.Fatal("a relabel and a move copy their objects, no other kind does")
	}
}
