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

// TestLabeledGaugesShareOneHeader guards gauges of one name with
// different labels, as for counters: one HELP and TYPE, then each labeled
// sample; and a gauge with no sample now described with no sample line.
func TestLabeledGaugesShareOneHeader(t *testing.T) {
	m := NewMetrics(Sources{Gauges: func() []Gauge {
		return []Gauge{
			{Name: "nopacs_lag_seconds", Help: "Lag.", Value: 90, Labels: []string{"tenant", "lab", "replica", "copy1"}},
			{Name: "nopacs_lag_seconds", Help: "Lag.", Value: 0.5, Labels: []string{"tenant", "lab", "replica", "copy2"}},
			{Name: "nopacs_share", Help: "Share.", Empty: true},
			{Name: "nopacs_age_seconds", Help: "Age.", Value: 3},
		}
	}})
	var b bytes.Buffer
	if err := m.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	if n := strings.Count(text, "# TYPE nopacs_lag_seconds gauge"); n != 1 {
		t.Errorf("the labeled gauge is described %d times, want once:\n%s", n, text)
	}
	for _, want := range []string{
		"nopacs_lag_seconds{tenant=\"lab\",replica=\"copy1\"} 90\n",
		"nopacs_lag_seconds{tenant=\"lab\",replica=\"copy2\"} 0.5\n",
		"# HELP nopacs_share Share.\n# TYPE nopacs_share gauge\n# HELP nopacs_age_seconds",
		"# TYPE nopacs_age_seconds gauge\nnopacs_age_seconds 3\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the text lacks %q:\n%s", want, text)
		}
	}
}
