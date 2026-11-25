package obs

import (
	"bytes"
	"strings"
	"testing"
)

// TestLabeledCountersShareOneHeader guards counters of one name with
// different labels: the text describes the metric once, then gives each
// labeled sample, as the text format requires.
func TestLabeledCountersShareOneHeader(t *testing.T) {
	m := NewMetrics(Sources{Counters: func() []Counter {
		return []Counter{
			{Name: "nopacs_x_total", Help: "Xs.", Value: 2, Labels: []string{"tenant", "lab", "storage", "copy1"}},
			{Name: "nopacs_x_total", Help: "Xs.", Value: 5, Labels: []string{"tenant", "lab", "storage", "copy2"}},
			{Name: "nopacs_y_total", Help: "Ys.", Value: 1},
		}
	}})
	var b bytes.Buffer
	if err := m.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	if n := strings.Count(text, "# TYPE nopacs_x_total counter"); n != 1 {
		t.Errorf("the labeled metric is described %d times, want once:\n%s", n, text)
	}
	for _, want := range []string{
		"nopacs_x_total{tenant=\"lab\",storage=\"copy1\"} 2\n",
		"nopacs_x_total{tenant=\"lab\",storage=\"copy2\"} 5\n",
		"# TYPE nopacs_y_total counter\nnopacs_y_total 1\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the text lacks %q:\n%s", want, text)
		}
	}
}
