package leaktest

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// knownLeaks are the surfaces that show an original identifier today, each
// with the defect in words. A surface listed here is skipped, so the rest of
// the test runs; one listed that shows nothing fails, so a fix removes its
// line. The group is the label up to the method, route or command. None
// leaks today.
var knownLeaks = map[string]string{}

// mustNotBeArchived are the originals no profile that de-identifies may leave
// in what it archives: if one is found there, the receiving side failed and
// the test has nothing to compare with.
var mustNotBeArchived = func() map[string]bool {
	out := map[string]bool{}
	for n := 1; n <= 9; n++ {
		name, id, other, acc, staff, priv, birth, _, _ := seedValues(n)
		for _, v := range []string{name, id, other[0], other[1], acc, staff, priv, birth} {
			out[v] = true
		}
	}
	return out
}()

// finding is one original value found on one surface.
type finding struct {
	label, who, value, form, around string
}

// search returns every needle found in every surface.
func search(items []surface, needles []needle) []finding {
	var out []finding
	for _, it := range items {
		low := strings.ToLower(it.text)
		for _, n := range needles {
			i := strings.Index(low, n.form)
			if i < 0 {
				continue
			}
			from, to := max(0, i-60), min(len(low), i+len(n.form)+60)
			out = append(out, finding{label: it.label, who: it.who, value: n.value, form: n.form, around: strings.ReplaceAll(it.text[from:to], "\n", " ")})
		}
	}
	return out
}

// groupOf returns the label of a surface up to its method, route or command:
// "api/GetScan/raw" is "api/GetScan", "cli/audit list --json stdout" is
// "cli/audit list".
func groupOf(label string) string {
	switch {
	case strings.HasPrefix(label, "api/"):
		return "api/" + strings.SplitN(strings.TrimPrefix(label, "api/"), "/", 2)[0]
	case strings.HasPrefix(label, "cli/"):
		l := strings.TrimPrefix(label, "cli/")
		l = strings.TrimSuffix(strings.TrimSuffix(l, " stdout"), " stderr")
		return "cli/" + strings.TrimSuffix(l, " --json")
	case strings.HasPrefix(label, "http/"):
		return "http/" + strings.SplitN(strings.TrimPrefix(label, "http/"), " ", 2)[0]
	}
	return label
}

// TestSearchFindsEveryVariant guards the search itself: a value is found in
// its plain form and in each variant a surface might show it in, so a clean
// run means something. It needs no server.
func TestSearchFindsEveryVariant(t *testing.T) {
	name, id, other, acc, staff, priv, birth, study, uids := seedValues(1)
	needles := needlesOf(seeds())
	for _, c := range []struct{ what, text string }{
		{"the name", `{"name":"` + name + `"}`},
		{"the name in lower case", strings.ToLower(name)},
		{"the name in upper case", strings.ToUpper(name)},
		{"the name with spaces for carets", strings.ReplaceAll(name, "^", " ")},
		{"the name as family, given", strings.ReplaceAll(name, "^", ", ")},
		{"the family name alone", strings.Split(name, "^")[0]},
		{"the Patient ID", "id=" + id},
		{"an other Patient ID", other[1]},
		{"the Accession Number in lower case", strings.ToLower(acc)},
		{"the staff name", "by " + staff + " today"},
		{"the private name", priv},
		{"the birth date", birth},
		{"the birth date with hyphens", birth[:4] + "-" + birth[4:6] + "-" + birth[6:]},
		{"the study date", study},
		{"the study date with hyphens", study[:4] + "-" + study[4:6] + "-" + study[6:]},
		{"a UID", "uid " + uids[2]},
		{"the name in a bytes field", base64.StdEncoding.EncodeToString([]byte("PN:" + name + ";"))},
		{"the Patient ID in a bytes field", base64.StdEncoding.EncodeToString([]byte("x" + id))},
	} {
		if got := search([]surface{{label: "x", text: c.text}}, needles); len(got) == 0 {
			t.Errorf("%s was not found in %q", c.what, c.text)
		}
	}
	if got := search([]surface{{label: "x", text: `{"subjectLabel":"sub-a1b2c3","modality":"CT"}`}}, needles); len(got) != 0 {
		t.Errorf("the search found %v in text with no original", got)
	}
}

// TestCallsMatchTheContract guards the table of calls: every request is read
// strictly by the contract's own message of its method, and every method of
// the API has a call, so a method added later is driven too. It needs no
// server.
func TestCallsMatchTheContract(t *testing.T) {
	w := &world{tenant: "lab1", other: "lab2", quarantined: []string{"q1", "q2"}, receptions: []string{"r1"}, jobID: "j1", holdID: "h1", own: []byte("x"), profile: profileDocument()}
	w.deid = scanRef{"sub-a", "ses-a", "scan-a", "i1", "1.2.3", "1.2.4"}
	w.raw = scanRef{"sub-b", "ses-b", "scan-b", "i2", "1.2.5", "1.2.6"}
	known := contract()
	covered := map[string]bool{}
	for _, c := range apiCalls(w) {
		m, ok := known[c.method]
		if !ok {
			t.Errorf("%s: the contract has no such method", c.label())
			continue
		}
		covered[c.method] = true
		if err := requestParses(m, c.body); err != nil {
			t.Errorf("%s: %v", c.label(), err)
		}
	}
	for name := range known {
		if !covered[name] {
			t.Errorf("no call of %s: add one to apiCalls", name)
		}
	}
}

// TestSeededIdentifiersReachNoSurface is the seeded-identifier test: a real
// server receives synthetic files, through a receiver that de-identifies
// (DIMSE and STOW-RS), through one that keeps files as received, and with two
// held back by the check after; masked keys of every grant level then call
// every method of the API, the DICOMweb and download routes and every command
// of the command line, with answers, refusals and not-found errors, as
// written and as JSON. No original value of any file may appear, plain or in
// a variant, in an answer, an error and its details, the server's log, the
// audit log (through the API and in the database), the event feed (through
// the API and in the database), or the command line's output.
func TestSeededIdentifiersReachNoSurface(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the programs and runs a server, receives files over the network and runs the command line a thousand times; left out under -short")
	}
	bin := buildBinaries(t)
	all := seeds()
	dir := t.TempDir()
	for i := range all {
		writeSeed(t, dir, &all[i])
	}
	s := newSite(t, bin)
	w := s.setUp(t, all)
	needles := needlesOf(all)
	needles = s.checkArchive(t, w, all, needles)

	rec := &recorder{}
	keysFrom := len(s.log.String())
	known := contract()
	start := time.Now()
	driveAPI(t, s, w, known, rec)
	driveHTTP(t, s, w, known, rec)
	t.Logf("the API and the routes: %v", time.Since(start).Round(time.Second))
	start = time.Now()
	f := writeCLIFiles(t, w, all)
	keys := []cliKey{{"no-key", ""}, {"bad-token", "nopacs_111111111111_1111111111111111111111111111111111111111111"}}
	for _, k := range w.keys {
		switch k.name {
		case "no-grant", "tenant-reader", "tenant-operator", "tenant-admin", "installation-admin":
			keys = append(keys, cliKey{k.name, k.secret})
		}
	}
	codes := runCLICases(t, bin, s.listen, w, f, keys, rec)
	t.Logf("the command line: %v", time.Since(start).Round(time.Second))
	leaves := guideLeaves(t, bin)
	covered := map[string]bool{}
	for n := range cliCases(w, f, "x") {
		covered[n] = true
	}
	for _, l := range leaves {
		if !covered[l] {
			t.Errorf("no command line case for %q: add one to cliCases", l)
		}
	}
	for n := range covered {
		if !contains(leaves, n) {
			t.Errorf("a case for %q, which the program's guide does not list", n)
		}
	}
	t.Logf("exit codes of the command line runs, by key: %v", codes)
	drainAudit(t, s, w, rec)
	s.stop()
	addLog(rec, s.log.String(), keysFrom)
	readDatabases(t, s, rec)

	rec.mu.Lock()
	items := append([]surface(nil), rec.items...)
	rec.mu.Unlock()
	dump(t, items)
	checkReached(t, items)
	control(t, s, w, needles)
	found := search(items, needles)
	byGroup := map[string][]finding{}
	groups := map[string]bool{}
	for _, it := range items {
		groups[groupOf(it.label)] = true
	}
	for _, x := range found {
		byGroup[groupOf(x.label)] = append(byGroup[groupOf(x.label)], x)
	}
	for g := range knownLeaks {
		groups[g] = true
	}
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		t.Run(g, func(t *testing.T) {
			shown := byGroup[g]
			reason, listed := knownLeaks[g]
			switch {
			case len(shown) == 0 && listed:
				t.Errorf("%s is listed as leaking (%s) and shows no original: remove it from knownLeaks", g, reason)
			case len(shown) > 0 && listed:
				t.Skipf("known leak: %s (%s)", reason, describe(shown))
			case len(shown) > 0:
				t.Errorf("%s shows an original value:\n%s", g, describe(shown))
			}
		})
	}
}

// describe lists findings, the first few of each place and value.
func describe(fs []finding) string {
	seen := map[string]bool{}
	var b strings.Builder
	for _, x := range fs {
		k := x.label + "|" + x.who + "|" + x.value
		if seen[k] {
			continue
		}
		seen[k] = true
		if len(seen) > 8 {
			b.WriteString("  ...\n")
			break
		}
		fmt.Fprintf(&b, "  %s as %s: %q (value %q) in ...%s...\n", x.label, x.who, x.form, x.value, x.around)
	}
	return b.String()
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
