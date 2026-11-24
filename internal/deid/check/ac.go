package check

import (
	"bytes"
	"context"
	"slices"
	"sort"
)

// matcher finds every pattern of a set in a text in one pass over it, the
// automaton of Aho and Corasick (Communications of the ACM 18(6), 1975): a
// trie of the patterns whose every node knows the longest proper suffix of
// its path that is also a path (fail) and the nearest such suffix that
// ends a pattern (dict). The work of a pass is the bytes of the text, plus
// the steps back along fail links, at most as many again, plus one per
// pattern ending at each position, which the caller's budget bounds.
//
// The trie is built level by level from the patterns in sorted order, so
// the children of a node are contiguous and sorted, and a node is a few
// numbers in flat slices: about 19 bytes a node, a node a byte of the
// patterns at most.
type matcher struct {
	// label is the byte on the edge into each node; first and count give
	// its children, nodes first to first+count-1.
	label []byte
	first []int32
	count []uint16
	fail  []int32
	// dict is the nearest node along fail links that ends a pattern, or
	// -1; pat the pattern that ends at the node, or -1.
	dict []int32
	pat  []int32
	// root is the full transition table of the root, the node most
	// visited.
	root [256]int32
	// lens is each pattern's length in bytes, longest the longest.
	lens    []int
	longest int
}

// child returns the child of node n by b, or -1.
func (m *matcher) child(n int32, b byte) int32 {
	lo, hi := int(m.first[n]), int(m.first[n])+int(m.count[n])
	i := lo + sort.Search(hi-lo, func(i int) bool { return m.label[lo+i] >= b })
	if i < hi && m.label[i] == b {
		return int32(i) //nolint:gosec // G115: nodes are bounded by the forms' bound, far below 2^31.
	}
	return -1
}

// newNode appends a node with the label b and returns it.
func (m *matcher) newNode(b byte) int32 {
	m.label = append(m.label, b)
	m.first = append(m.first, 0)
	m.count = append(m.count, 0)
	m.fail = append(m.fail, 0)
	m.dict = append(m.dict, -1)
	m.pat = append(m.pat, -1)
	return int32(len(m.label) - 1) //nolint:gosec // G115: nodes are bounded by the forms' bound, far below 2^31.
}

// newMatcher builds the automaton of patterns, which the caller has made
// distinct and none empty; pattern i is reported as i.
func newMatcher(patterns [][]byte) *matcher {
	m := &matcher{lens: make([]int, len(patterns))}
	order := make([]int32, len(patterns))
	for i, p := range patterns {
		m.lens[i] = len(p)
		m.longest = max(m.longest, len(p))
		order[i] = int32(i) //nolint:gosec // G115: the patterns are bounded by the forms' bound.
	}
	slices.SortFunc(order, func(a, b int32) int { return bytes.Compare(patterns[a], patterns[b]) })
	m.newNode(0)
	// Each node of a level holds the range of sorted patterns that begin
	// with its path; a shorter pattern sorts before the longer ones it
	// begins, so the one ending at the node comes first in its range.
	type span struct{ lo, hi int }
	level, spans := []int32{0}, []span{{0, len(order)}}
	for depth := 0; len(level) > 0; depth++ {
		var next []int32
		var nextSpans []span
		for k, n := range level {
			i, hi := spans[k].lo, spans[k].hi
			for i < hi && len(patterns[order[i]]) == depth {
				m.pat[n] = order[i]
				i++
			}
			m.first[n] = int32(len(m.label)) //nolint:gosec // G115: as newNode.
			for i < hi {
				b := patterns[order[i]][depth]
				j := i
				for j < hi && patterns[order[j]][depth] == b {
					j++
				}
				next = append(next, m.newNode(b))
				nextSpans = append(nextSpans, span{i, j})
				i = j
			}
			m.count[n] = uint16(len(m.label) - int(m.first[n])) //nolint:gosec // G115: at most 256 children.
		}
		level, spans = next, nextSpans
	}
	for c := m.first[0]; c < m.first[0]+int32(m.count[0]); c++ {
		m.root[m.label[c]] = c
	}
	// Nodes are numbered breadth first, so a node's fail target, which is
	// shallower, is finished before it.
	for n := int32(0); n < int32(len(m.label)); n++ { //nolint:gosec // G115: as newNode.
		for c := m.first[n]; c < m.first[n]+int32(m.count[n]); c++ {
			if n == 0 {
				m.fail[c] = 0
			} else {
				f := m.fail[n]
				for {
					if g := m.child(f, m.label[c]); g >= 0 {
						m.fail[c] = g
						break
					}
					if f == 0 {
						m.fail[c] = 0
						break
					}
					f = m.fail[f]
				}
			}
			if fn := m.fail[c]; m.pat[fn] >= 0 {
				m.dict[c] = fn
			} else {
				m.dict[c] = m.dict[fn]
			}
		}
	}
	return m
}

// errBudget is a pass that would pass its budget of work.
type errBudget struct{}

func (errBudget) Error() string { return "deid check: the search passes its bound" }

// ctxEvery is how many bytes a pass reads between looks at its context.
const ctxEvery = 64 << 10

// scan passes over text once and calls found with each pattern ending at
// each position: the pattern and the offset one past its end. Each pattern
// reported costs one unit of *budget; past it the pass ends with
// errBudget and the offset it had read to. The steps of the automaton are
// not charged: there are at most two a byte. ctx is read every ctxEvery
// bytes.
func (m *matcher) scan(ctx context.Context, text []byte, budget *int64, found func(pat, end int)) (int, error) {
	return m.scanRange(ctx, text, 0, len(text), budget, found)
}

// scanRange is scan over text[:to] that reports, and charges, only the
// patterns ending past from: the bytes before from are read for the state
// of the automaton, as a window's overlap with the one before is, whose
// matches that window reported.
func (m *matcher) scanRange(ctx context.Context, text []byte, from, to int, budget *int64, found func(pat, end int)) (int, error) {
	cur := int32(0)
	for i, b := range text[:to] {
		if i%ctxEvery == 0 {
			if err := ctx.Err(); err != nil {
				return i, err
			}
		}
		for {
			if cur == 0 {
				cur = m.root[b]
				break
			}
			if c := m.child(cur, b); c >= 0 {
				cur = c
				break
			}
			cur = m.fail[cur]
		}
		if i < from {
			continue
		}
		n := cur
		if m.pat[n] < 0 {
			n = m.dict[n]
		}
		for n > 0 {
			*budget--
			found(int(m.pat[n]), i+1)
			n = m.dict[n]
		}
		if *budget < 0 {
			return i + 1, errBudget{}
		}
	}
	return to, nil
}
