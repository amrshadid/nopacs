package query

import "slices"

// Fields returns the fields a condition compares, each once as FieldString
// writes it, in order: what a log records of a search without its values.
func Fields(n Node) []string {
	seen := map[string]bool{}
	var walk func(Node)
	walk = func(n Node) {
		switch n := n.(type) {
		case *Leaf:
			seen[FieldString(n.Field)] = true
		case *And:
			for _, c := range n.Children {
				walk(c)
			}
		case *Or:
			for _, c := range n.Children {
				walk(c)
			}
		case *Not:
			walk(n.Child)
		case *Has:
			walk(n.Child)
		}
	}
	if n != nil {
		walk(n)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
