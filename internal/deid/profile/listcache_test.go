package profile_test

import (
	"fmt"
	"sync"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// listDoc is a document of one list of terms and a clean naming it.
func listDoc(terms ...string) *v1.ProfileDocument {
	return &v1.ProfileDocument{
		TextLists: []*v1.TextList{{Name: "staff", Version: 1, Terms: terms}},
		Rules:     []*v1.ProfileRule{rl("d", kw("StudyDescription"), cleanLists("staff"))},
	}
}

// TestListCacheHoldsStoredVersionsOnly guards the cache of compiled lists:
// a document validated (Expand, Warnings), stacked in layers or compiled
// once (CompileOneOff, a trial, a description) never enters it; a stored version compiled twice shares
// one set of lists; the cache counts their automata, and past its bound
// in bytes drops the least recently used, still answering each version's
// own lists.
func TestListCacheHoldsStoredVersionsOnly(t *testing.T) {
	cat := testCatalog(t)
	profile.SetListCacheBytes(profile.DefaultListCacheBytes)
	t.Cleanup(func() { profile.SetListCacheBytes(profile.DefaultListCacheBytes) })
	_, _, before := profile.ListCacheBytes()
	e := mustExpand(t, listDoc("Unique Cachename One"), cat)
	if _, err := profile.CompileOneOff(e); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Warnings(e); err != nil {
		t.Fatal(err)
	}
	if _, problems := profile.Stack(profile.Layer{Kind: profile.LayerTenant, Name: "a@1", Expanded: e},
		profile.Layer{Kind: profile.LayerReceiver, Name: "b@1", Expanded: mustExpand(t, &v1.ProfileDocument{
			TextLists: []*v1.TextList{{Name: "sites", Version: 1, Terms: []string{"Unique Cachename Two"}}}}, cat)}); problems != nil {
		t.Fatal(problems)
	}
	if _, _, n := profile.ListCacheBytes(); n != before {
		t.Errorf("a document's lists, validated, compiled once or stacked, entered the cache: %d entries, %d before", n, before)
	}
	a, err := profile.Compile(e)
	if err != nil {
		t.Fatal(err)
	}
	b, err := profile.Compile(e)
	if err != nil {
		t.Fatal(err)
	}
	total, _, n := profile.ListCacheBytes()
	if a.TextLists() != b.TextLists() || n != before+1 || total < a.TextLists().Retained() || a.TextLists().Retained() == 0 {
		t.Errorf("a stored version's lists: shared %v, %d entries, %d bytes counted", a.TextLists() == b.TextLists(), n, total)
	}
	profile.SetListCacheBytes(a.TextLists().Retained() * 3)
	for i := range 10 {
		p, err := profile.Compile(mustExpand(t, listDoc(fmt.Sprintf("Cached Name %02d", i)), cat))
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		p.TextLists().EachTerm(func(_ int, f string, _ bool) { got = f })
		if got != profile.FoldText(fmt.Sprintf("Cached Name %02d", i)) {
			t.Errorf("version %d compiled with lists %q", i, got)
		}
	}
	if total, bound, _ := profile.ListCacheBytes(); total > bound {
		t.Errorf("the cache holds %d bytes past its bound of %d", total, bound)
	}
}

// TestListCacheUnderConcurrency guards the cache with many versions
// compiled at once, past its size: each Program has its own version's
// terms, never another's.
func TestListCacheUnderConcurrency(t *testing.T) {
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				name := fmt.Sprintf("term%02dx%02d", g, i%12)
				e, problems := profile.Expand(listDoc(name), profile.Shipped())
				if problems != nil {
					t.Error(problems)
					return
				}
				p, err := profile.Compile(e)
				if err != nil {
					t.Error(err)
					return
				}
				got := ""
				p.TextLists().EachTerm(func(_ int, f string, _ bool) { got = f })
				if got != profile.FoldText(name) {
					t.Errorf("compiled lists hold %q, want %q", got, profile.FoldText(name))
				}
			}
		}()
	}
	wg.Wait()
}
