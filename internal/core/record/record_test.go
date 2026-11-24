package record

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	t0   = time.Date(2026, 11, 20, 8, 30, 0, 123456000, time.UTC)
	sumA = strings.Repeat("a", 64)
	sumB = strings.Repeat("b", 64)
)

// opID is an operation id: a ULID in lowercase.
const opID = "01jd5x7k2m3n4p5q6r7s8t9v0w"

func scanOf() Scan {
	return Scan{Subject: "SUB-001", Session: "20240131", Scan: "3-1.2.4", StudyUID: "1.2", SeriesUID: "1.2.4"}
}

// otherScan is a second scan of scanOf's session.
func otherScan() Scan {
	s := scanOf()
	s.Scan, s.SeriesUID = "4-1.2.5", "1.2.5"
	return s
}

// otherSession is a scan of another session of scanOf's subject.
func otherSession() Scan {
	return Scan{Subject: "SUB-001", Session: "20250201", Scan: "2-1.3.2", StudyUID: "1.3", SeriesUID: "1.3.2"}
}

func deleteRec() Correction {
	return Correction{Format: Format, ID: opID, Kind: KindDelete, Level: LevelScan, State: StatePrepared, ReasonSHA256: ReasonDigest("consent withdrawn"),
		Created: t0, Scans: []Scan{scanOf()}}
}

func published(c Correction, s State) Correction {
	c.State = s
	c.CommittedAt = t0.Add(time.Minute)
	c.PurgeAfter = t0.Add(7 * 24 * time.Hour)
	return c
}

// TestCorrectionRoundTrip guards that every kind and state a record may
// have is written and read back to an equal record, with its times in UTC.
func TestCorrectionRoundTrip(t *testing.T) {
	instDel := deleteRec()
	instDel.Level, instDel.Instances = LevelInstance, []string{"1.2.5.1", "1.2.5.2"}
	sessionDel := deleteRec()
	sessionDel.Level, sessionDel.Scans = LevelSession, []Scan{scanOf(), otherScan()}
	subjectDel := deleteRec()
	subjectDel.Level, subjectDel.Scans = LevelSubject, []Scan{scanOf(), otherScan(), otherSession()}
	relabel := deleteRec()
	relabel.Kind, relabel.Level, relabel.Relabel = KindRelabel, "", &Relabel{Level: LevelSubject, From: "SUB-001", To: "SUB-002"}
	replace := deleteRec()
	replace.Kind, replace.Level, replace.Replace = KindReplace, "", &Replacement{SOPInstanceUID: "1.2.5.1", OldRevision: 1, NewRevision: 2, OldSHA256: sumA, NewSHA256: sumB}
	purge := &Purge{Removed: 3, Hidden: 1, Retained: 2, Unknown: 0, RetainUntil: t0.Add(30 * 24 * time.Hour)}
	purged := published(deleteRec(), StatePurged)
	purged.Purge = purge
	purging := published(replace, StatePurging)
	purging.Purge = &Purge{Removed: 1}
	// A replace that repairs a damaged instance puts back the bytes it was
	// archived with: the two checksums are equal.
	repair := deleteRec()
	repair.Kind, repair.Level, repair.Replace = KindReplace, "", &Replacement{SOPInstanceUID: "1.2.5.1", OldRevision: 1, NewRevision: 2, OldSHA256: sumA, NewSHA256: sumA}
	for name, c := range map[string]Correction{
		"prepared delete": deleteRec(), "instance delete": instDel, "session delete": sessionDel, "subject delete": subjectDel,
		"relabel": relabel, "replace": replace, "repair": repair,
		"committed": published(deleteRec(), StateCommitted), "undone": published(deleteRec(), StateUndone),
		"abandoned":       func() Correction { c := deleteRec(); c.State = StateAbandoned; return c }(),
		"purging replace": purging, "purged": purged, "committed relabel": published(relabel, StateCommitted),
	} {
		b, err := encode(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		back, err := decodeRec(bytes.NewReader(b))
		if err != nil || back.KeyVersion != 2 || len(back.MAC) != 64 {
			t.Fatalf("%s: read back %+v, %v", name, back, err)
		}
		back.KeyVersion, back.MAC = 0, ""
		if !reflect.DeepEqual(back, c) {
			t.Fatalf("%s: read back %+v; wrote %+v", name, back, c)
		}
		if key, err := c.Key(); err != nil || key != "meta/.corrections/"+opID+".json" {
			t.Fatalf("%s: key %q %v", name, key, err)
		}
	}
	local := deleteRec()
	local.Created = t0.In(time.FixedZone("x", 3600))
	b, err := encode(local)
	if err != nil || !bytes.Contains(b, []byte(`"created":"2026-11-20T08:30:00.123456Z"`)) {
		t.Fatalf("a time in another zone is written in UTC: %s %v", b, err)
	}
}

// TestCorrectionRefuses guards each rule of the correction record: what
// Encode will not write and Decode will not take.
func TestCorrectionRefuses(t *testing.T) {
	mut := func(f func(*Correction)) Correction { c := deleteRec(); f(&c); return c }
	repl := func(f func(*Correction)) Correction {
		c := deleteRec()
		c.Kind, c.Level, c.Replace = KindReplace, "", &Replacement{SOPInstanceUID: "1.2.5.1", OldRevision: 1, NewRevision: 2, OldSHA256: sumA, NewSHA256: sumB}
		f(&c)
		return c
	}
	rel := func(f func(*Correction)) Correction {
		c := deleteRec()
		c.Kind, c.Level, c.Relabel = KindRelabel, "", &Relabel{Level: LevelScan, From: "3-1.2.4", To: "b"}
		f(&c)
		return c
	}
	two := func(level string, s Scan) Correction {
		c := deleteRec()
		c.Level, c.Scans = level, append(c.Scans, s)
		return c
	}
	set := func(s Scan, f func(*Scan)) Scan { f(&s); return s }
	two2 := func() []Scan { return []Scan{scanOf(), otherSession()} }
	if _, err := encode(rel(func(*Correction) {})); err != nil {
		t.Fatalf("the relabel the cases change is refused: %v", err)
	}
	for name, c := range map[string]Correction{
		"format":             mut(func(c *Correction) { c.Format = 2 }),
		"id empty":           mut(func(c *Correction) { c.ID = "" }),
		"id upper":           mut(func(c *Correction) { c.ID = strings.ToUpper(opID) }),
		"id slash":           mut(func(c *Correction) { c.ID = "a/b" }),
		"id not a ulid":      mut(func(c *Correction) { c.ID = "op-0001" }),
		"id letter u":        mut(func(c *Correction) { c.ID = opID[:25] + "u" }),
		"id past 128 bits":   mut(func(c *Correction) { c.ID = "8" + opID[1:] }),
		"no level":           mut(func(c *Correction) { c.Level = "" }),
		"level tenant":       mut(func(c *Correction) { c.Level = "tenant" }),
		"instance level":     mut(func(c *Correction) { c.Level = LevelInstance }),
		"instances scan":     mut(func(c *Correction) { c.Instances = []string{"1.2.5.1"} }),
		"scan level two":     two(LevelScan, otherScan()),
		"session level two":  two(LevelSession, otherSession()),
		"subject level two":  two(LevelSubject, Scan{Subject: "SUB-002", Session: "s", Scan: "c", StudyUID: "1.4", SeriesUID: "1.4.1"}),
		"study uid":          mut(func(c *Correction) { c.Scans[0].StudyUID = "" }),
		"session two study":  two(LevelSession, set(otherScan(), func(s *Scan) { s.StudyUID = "1.9" })),
		"study two sessions": two(LevelSubject, set(otherSession(), func(s *Scan) { s.StudyUID = "1.2" })),
		"series twice":       two(LevelSession, set(otherScan(), func(s *Scan) { s.SeriesUID = "1.2.4" })),
		"scan label twice":   two(LevelSession, set(otherScan(), func(s *Scan) { s.Scan = "3-1.2.4" })),
		"kind":               mut(func(c *Correction) { c.Kind = "purge" }),
		"state":              mut(func(c *Correction) { c.State = "done" }),
		"undone relabel":     rel(func(c *Correction) { *c = published(*c, StateUndone) }),
		"reason":             mut(func(c *Correction) { c.ReasonSHA256 = "ABC" }),
		"created zero":       mut(func(c *Correction) { c.Created = time.Time{} }),
		"prepared committed": mut(func(c *Correction) { c.CommittedAt = t0 }),
		"prepared purge":     mut(func(c *Correction) { c.PurgeAfter = t0 }),
		"committed no time":  mut(func(c *Correction) { c.State = StateCommitted }),
		"purge before":       mut(func(c *Correction) { *c = published(*c, StateCommitted); c.PurgeAfter = c.CommittedAt.Add(-1) }),
		"committed early":    mut(func(c *Correction) { *c = published(*c, StateCommitted); c.CommittedAt = t0.Add(-time.Second) }),
		"no scans":           mut(func(c *Correction) { c.Scans = nil }),
		"scan label":         mut(func(c *Correction) { c.Scans[0].Scan = "../x" }),
		"series uid":         mut(func(c *Correction) { c.Scans[0].SeriesUID = "1.02" }),
		"delete relabel":     mut(func(c *Correction) { c.Relabel = &Relabel{Level: "scan", From: "3-1.2.4", To: "b"} }),
		"instance order":     mut(func(c *Correction) { c.Level, c.Instances = LevelInstance, []string{"1.3", "1.2"} }),
		"instance twice":     mut(func(c *Correction) { c.Level, c.Instances = LevelInstance, []string{"1.2", "1.2"} }),
		"instance uid":       mut(func(c *Correction) { c.Level, c.Instances = LevelInstance, []string{"x"} }),
		"instances scans": mut(func(c *Correction) {
			c.Level, c.Instances, c.Scans = LevelInstance, []string{"1.2"}, append(c.Scans, otherScan())
		}),
		"relabel missing":    mut(func(c *Correction) { c.Kind, c.Level = KindRelabel, "" }),
		"relabel level":      rel(func(c *Correction) { c.Relabel.Level = "tenant" }),
		"relabel same":       rel(func(c *Correction) { c.Relabel.To = "3-1.2.4" }),
		"relabel label":      rel(func(c *Correction) { c.Relabel.To = "a/b" }),
		"relabel instances":  rel(func(c *Correction) { c.Instances = []string{"1.2"} }),
		"relabel from":       rel(func(c *Correction) { c.Relabel.From = "a" }),
		"relabel from level": rel(func(c *Correction) { c.Relabel.Level, c.Relabel.From = LevelSession, "3-1.2.4" }),
		"relabel two scans":  rel(func(c *Correction) { c.Scans = append(c.Scans, otherScan()) }),
		"relabel two sess":   rel(func(c *Correction) { c.Relabel.Level, c.Relabel.From, c.Scans = LevelSession, "20240131", two2() }),
		"relabel with level": rel(func(c *Correction) { c.Level = LevelScan }),
		"replace level":      repl(func(c *Correction) { c.Level = LevelInstance }),
		"replace missing":    mut(func(c *Correction) { c.Kind = KindReplace }),
		"replace skip":       repl(func(c *Correction) { c.Replace.NewRevision = 3 }),
		"replace rev 0":      repl(func(c *Correction) { c.Replace.OldRevision, c.Replace.NewRevision = 0, 1 }),
		"replace rev max":    repl(func(c *Correction) { c.Replace.OldRevision, c.Replace.NewRevision = 1000000, 1000001 }),
		"replace sum":        repl(func(c *Correction) { c.Replace.OldSHA256 = "zz" }),
		"replace uid":        repl(func(c *Correction) { c.Replace.SOPInstanceUID = "" }),
		"replace scans":      repl(func(c *Correction) { c.Scans = append(c.Scans, scanOf()) }),
		"purged no purge":    mut(func(c *Correction) { *c = published(*c, StatePurged) }),
		"purge prepared":     mut(func(c *Correction) { c.Purge = &Purge{} }),
		"purge negative":     mut(func(c *Correction) { *c = published(*c, StatePurging); c.Purge = &Purge{Hidden: -1} }),
	} {
		if b, err := encode(c); err == nil || !errors.Is(err, ErrFormat) {
			t.Errorf("%s: wrote %s, %v", name, b, err)
		}
	}
}

// TestDecodeRefuses guards the reader: unknown fields, trailing bytes,
// truncation, other types, and a record past the bound are refused.
func TestDecodeRefuses(t *testing.T) {
	good, err := encode(deleteRec())
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"empty":     "",
		"cut":       string(good[:len(good)/2]),
		"trailing":  string(good) + "{}",
		"two":       string(good) + string(good),
		"unknown":   strings.Replace(string(good), `"format":1`, `"format":1,"extra":true`, 1),
		"duplicate": strings.Replace(string(good), `"format":1`, `"format":1,"format":2`, 1),
		"array":     "[]",
		"null":      "null",
		"too long":  string(good) + strings.Repeat(" ", MaxBytes),
		"bad value": strings.Replace(string(good), `"state":"prepared"`, `"state":"gone"`, 1),
		"offset":    strings.Replace(string(good), "123456Z", "123456+01:00", 1),
		"number id": strings.Replace(string(good), `"id":"`+opID+`"`, `"id":1`, 1),
		"other id":  strings.Replace(string(good), `"id":"`+opID+`"`, `"id":"op-0001"`, 1),
		"no level":  strings.Replace(string(good), `"level":"scan",`, ``, 1),
		"no study":  strings.Replace(string(good), `"study_uid":"1.2",`, ``, 1),
	} {
		if c, err := decodeRec(strings.NewReader(in)); err == nil {
			t.Errorf("%s: read %+v", name, c)
		} else if !errors.Is(err, ErrFormat) {
			t.Errorf("%s: %v does not wrap ErrFormat", name, err)
		}
	}
	// Trailing whitespace is not another value.
	if _, err := decodeRec(strings.NewReader(string(good) + "\n\n")); err != nil {
		t.Errorf("trailing newlines: %v", err)
	}
}

// TestStateMoves guards the state machine's forward moves and nothing else.
func TestStateMoves(t *testing.T) {
	all := []State{StatePrepared, StateCommitted, StatePurging, StatePurged, StateUndone, StateAbandoned}
	allowed := map[[2]State]bool{
		{StatePrepared, StateCommitted}: true, {StatePrepared, StateAbandoned}: true,
		{StateCommitted, StatePurging}: true, {StateCommitted, StateUndone}: true,
		{StatePurging, StatePurged}: true,
	}
	for _, from := range all {
		for _, to := range all {
			if got, want := from.CanFollow(to, KindDelete), allowed[[2]State{from, to}]; got != want {
				t.Errorf("%s to %s (delete): %v, want %v", from, to, got, want)
			}
		}
	}
	if StateCommitted.CanFollow(StateUndone, KindReplace) || StateCommitted.CanFollow(StateUndone, KindRelabel) {
		t.Error("only a delete is undone")
	}
	if State("x").Valid() || !StatePurged.Published() || StateUndone.Published() || StatePrepared.Published() {
		t.Error("Valid or Published is wrong")
	}
}

// TestStateAfter guards After, which says a record is ahead of its row: a
// state reached from the other by one or more forward moves, for the kind
// (undone only for a delete), and no other pair, and Rank's order.
func TestStateAfter(t *testing.T) {
	all := []State{StatePrepared, StateCommitted, StatePurging, StatePurged, StateUndone, StateAbandoned, "x"}
	ahead := map[[2]State]bool{
		{StateCommitted, StatePrepared}: true, {StateAbandoned, StatePrepared}: true,
		{StatePurging, StatePrepared}: true, {StatePurging, StateCommitted}: true,
		{StatePurged, StatePrepared}: true, {StatePurged, StateCommitted}: true, {StatePurged, StatePurging}: true,
		{StateUndone, StatePrepared}: true, {StateUndone, StateCommitted}: true,
	}
	for _, s := range all {
		for _, prev := range all {
			if got, want := s.After(prev, KindDelete), ahead[[2]State{s, prev}]; got != want {
				t.Errorf("%s after %s (delete): %v, want %v", s, prev, got, want)
			}
			if s == StateUndone && s.After(prev, KindReplace) {
				t.Errorf("undone after %s for a replace", prev)
			}
			if ahead[[2]State{s, prev}] && s.Rank() <= prev.Rank() {
				t.Errorf("rank of %s (%d) is not past %s (%d)", s, s.Rank(), prev, prev.Rank())
			}
		}
	}
	if State("x").Rank() != -1 {
		t.Error("a state that is not one has no rank")
	}
}

func ownerRec() Owner {
	return Owner{Format: Format, Installation: "inst_A-1", Tenant: "lab1", Generation: 3, Written: t0}
}

// TestOwnerRoundTrip guards the owner record's write and read back, its key
// and which generation wins.
func TestOwnerRoundTrip(t *testing.T) {
	o := ownerRec()
	b, err := EncodeOwner(o, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeOwner(bytes.NewReader(b))
	if err != nil || back.KeyVersion != 2 || len(back.MAC) != 64 || o.Key() != "meta/.owner.json" {
		t.Fatalf("%+v %v %q", back, err, o.Key())
	}
	if err := VerifyOwner(back, testKeys); err != nil {
		t.Fatalf("a record just written does not verify: %v", err)
	}
	back.KeyVersion, back.MAC = 0, ""
	if back != o {
		t.Fatalf("read back %+v, wrote %+v", back, o)
	}
	later := o
	later.Generation = 4
	if !later.Supersedes(o) || o.Supersedes(later) || o.Supersedes(o) {
		t.Error("the higher generation supersedes, an equal one does not")
	}
}

// TestOwnerRefuses guards each rule of the owner record and the reader's
// refusals.
func TestOwnerRefuses(t *testing.T) {
	mut := func(f func(*Owner)) Owner { o := ownerRec(); f(&o); return o }
	for name, o := range map[string]Owner{
		"format":       mut(func(o *Owner) { o.Format = 0 }),
		"installation": mut(func(o *Owner) { o.Installation = "a b" }),
		"empty inst":   mut(func(o *Owner) { o.Installation = "" }),
		"long tenant":  mut(func(o *Owner) { o.Tenant = strings.Repeat("a", 65) }),
		"tenant dot":   mut(func(o *Owner) { o.Tenant = "a.b" }),
		"generation 0": mut(func(o *Owner) { o.Generation = 0 }),
		"generation -": mut(func(o *Owner) { o.Generation = -1 }),
		"generation >": mut(func(o *Owner) { o.Generation = MaxGeneration + 1 }),
		"written":      mut(func(o *Owner) { o.Written = time.Time{} }),
		"zone":         mut(func(o *Owner) { o.Written = t0.In(time.FixedZone("x", 60)).Add(0) }),
	} {
		if name == "zone" {
			// Encode converts to UTC, so a zone alone is not refused there.
			if _, err := EncodeOwner(o, testKeys); err != nil {
				t.Errorf("zone: %v", err)
			}
			continue
		}
		if b, err := EncodeOwner(o, testKeys); err == nil || !errors.Is(err, ErrFormat) {
			t.Errorf("%s: wrote %s, %v", name, b, err)
		}
	}
	good, _ := EncodeOwner(ownerRec(), testKeys)
	for name, in := range map[string]string{
		"empty":    "",
		"trailing": string(good) + "x",
		"unknown":  strings.Replace(string(good), `"tenant"`, `"extra":1,"tenant"`, 1),
		"offset":   strings.Replace(string(good), "123456Z", "123456+01:00", 1),
		"float":    strings.Replace(string(good), `"generation":3`, `"generation":3.5`, 1),
	} {
		if o, err := DecodeOwner(strings.NewReader(in)); err == nil {
			t.Errorf("%s: read %+v", name, o)
		}
	}
}

// FuzzDecode guards both readers: they never panic, and whatever a reader
// accepts is written back to bytes a reader accepts again, equal.
func FuzzDecode(f *testing.F) {
	c, _ := encode(deleteRec())
	o, _ := EncodeOwner(ownerRec(), testKeys)
	f.Add(c)
	f.Add(o)
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if c, err := decodeRec(bytes.NewReader(b)); err == nil {
			out, err := encode(c)
			if err != nil {
				t.Fatalf("accepted but not writable: %v", err)
			}
			if back, err := decodeRec(bytes.NewReader(out)); err != nil || !reflect.DeepEqual(back, c) {
				t.Fatalf("not stable: %v", err)
			}
		}
		if o, err := DecodeOwner(bytes.NewReader(b)); err == nil {
			out, err := EncodeOwner(o, testKeys)
			if err != nil {
				t.Fatalf("accepted but not writable: %v", err)
			}
			back, err := DecodeOwner(bytes.NewReader(out))
			if err != nil || VerifyOwner(back, testKeys) != nil {
				t.Fatalf("not stable: %v", err)
			}
			back.KeyVersion, back.MAC, o.KeyVersion, o.MAC = 0, "", 0, ""
			if back != o {
				t.Fatalf("not stable: %+v", back)
			}
		}
	})
}
