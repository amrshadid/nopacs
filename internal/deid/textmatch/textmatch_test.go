package textmatch

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestScanReportsEveryPattern guards the automaton: every pattern ending at
// every position is reported, patterns inside and overlapping others
// included, with the offset one past its end, compared with a search of
// each pattern at each offset; and ScanRange reports only the matches
// ending past from.
func TestScanReportsEveryPattern(t *testing.T) {
	patterns := []string{"HE", "SHE", "HIS", "HERS", "E", "SHESH"}
	text := "USHERSHESHIS"
	bs := make([][]byte, len(patterns))
	for i, p := range patterns {
		bs[i] = []byte(p)
	}
	m := New(bs)
	type hit struct{ pat, end int }
	var got []hit
	budget := int64(1000)
	if _, err := m.Scan(context.Background(), []byte(text), &budget, func(p, e int) { got = append(got, hit{p, e}) }); err != nil {
		t.Fatal(err)
	}
	var want []hit
	for end := 1; end <= len(text); end++ {
		for i, p := range patterns {
			if strings.HasSuffix(text[:end], p) {
				want = append(want, hit{i, end})
			}
		}
	}
	key := func(a, b hit) int {
		if a.end != b.end {
			return a.end - b.end
		}
		return a.pat - b.pat
	}
	slices.SortFunc(got, key)
	slices.SortFunc(want, key)
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if m.Len(3) != 4 || m.Longest() != 5 || m.Nodes() < 2 {
		t.Errorf("Len %d Longest %d Nodes %d", m.Len(3), m.Longest(), m.Nodes())
	}
	var late []hit
	if _, err := m.ScanRange(context.Background(), []byte(text), 9, len(text), &budget, func(p, e int) { late = append(late, hit{p, e}) }); err != nil {
		t.Fatal(err)
	}
	for _, h := range late {
		if h.end <= 9 {
			t.Errorf("a match ending at %d reported from 9", h.end)
		}
	}
}

// TestScanStopsAtItsBudget guards the budget: a pass whose reports pass
// it ends with ErrBudget, and a canceled context ends a pass.
func TestScanStopsAtItsBudget(t *testing.T) {
	m := New([][]byte{[]byte("A"), []byte("AA"), []byte("AAA")})
	budget := int64(10)
	if _, err := m.Scan(context.Background(), []byte(strings.Repeat("A", 100)), &budget, func(int, int) {}); !errors.As(err, new(ErrBudget)) {
		t.Errorf("past the budget: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	budget = 1 << 20
	if _, err := m.Scan(ctx, []byte("AAAA"), &budget, func(int, int) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled pass: %v", err)
	}
}
