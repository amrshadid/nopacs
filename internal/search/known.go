package search

// Known reports whether keyword names an attribute the search tables hold.
func Known(keyword string) bool {
	for _, a := range attributes {
		if a.Keyword == keyword {
			return true
		}
	}
	return false
}
