package tui

import "slices"

// basket holds the catalog rows a person marked, one tenant's at a time, for a
// batch delete to take as its targets. It is only ever used from the program's
// one update loop.
type basket struct {
	tenant string
	refs   []string
}

// toggle marks a reference (subject, subject/session or subject/session/scan)
// of a tenant, or unmarks it when it is marked, and says whether it is marked
// now. Marking under another tenant starts the basket afresh: a batch names
// one tenant's targets.
func (b *basket) toggle(tenant, ref string) bool {
	if b.tenant != tenant {
		b.tenant, b.refs = tenant, nil
	}
	if i := slices.Index(b.refs, ref); i >= 0 {
		b.refs = slices.Delete(b.refs, i, i+1)
		return false
	}
	b.refs = append(b.refs, ref)
	return true
}

// marked is the references marked for a tenant, in the order they were marked.
func (b *basket) marked(tenant string) []string {
	if b.tenant != tenant {
		return nil
	}
	return slices.Clone(b.refs)
}

// clear forgets the marks.
func (b *basket) clear() { b.tenant, b.refs = "", nil }
