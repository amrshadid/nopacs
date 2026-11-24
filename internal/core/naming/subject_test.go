package naming

import (
	"testing"
)

// TestHashedSubject guards the forward lookup of a subject's label: for a
// rule whose subject template is literal text and keyed hashes of one
// attribute, HashedSubject gives exactly the label Labels gives a file with
// that value; a rule whose subject reads another attribute, a pattern's
// part or a plain value answers false; an empty value is refused as Labels
// refuses it.
func TestHashedSubject(t *testing.T) {
	h := testHasher(t)
	a := studyAttrs(t)
	pid := kw(t, "PatientID")
	for _, c := range []struct {
		subject string
		ok      bool
	}{
		{"sub-{#PatientID}", true},
		{"p{#(0010,0020)}-x", true},
		{"{#PatientID}{#PatientID}", true},
		{"sub-{PatientID}", false},
		{"sub-{#PatientName}", false},
		{"sub-{#PatientID}-{StudyDate}", false},
	} {
		r, err := Parse(Spec{Subject: c.subject, Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}"})
		if err != nil {
			t.Fatalf("%s: %v", c.subject, err)
		}
		got, ok, err := r.HashedSubject(pid, a[pid], h)
		if err != nil || ok != c.ok {
			t.Errorf("%s: ok %v, %v; want ok %v", c.subject, ok, err, c.ok)
			continue
		}
		if !ok {
			continue
		}
		want, err := r.Labels(a, h)
		if err != nil {
			t.Fatal(err)
		}
		if got != want.Subject {
			t.Errorf("%s: HashedSubject %q, Labels %q", c.subject, got, want.Subject)
		}
	}
	if _, ok, err := Default().HashedSubject(pid, "", h); !ok || err == nil {
		t.Errorf("an empty value: ok %v, %v; want refused", ok, err)
	}
}
