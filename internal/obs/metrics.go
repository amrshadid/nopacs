package obs

import (
	"bufio"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DurationBuckets are the upper bounds, in seconds, of the request duration
// histogram. They are fixed, so a dashboard built on them keeps working:
// from a millisecond (a refusal answered from the master's cache) to the
// 30-second default deadline, roughly 2.5 times apart.
var DurationBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Counter is one counter read from its owner at the moment of a scrape.
type Counter struct {
	// Name is the metric's name, ending in _total; it comes from a fixed
	// list in the server, never from a request.
	Name string
	// Help says what is counted, as a sentence.
	Help string
	// Value is the count.
	Value uint64
}

// Gauge is one value read from its owner at the moment of a scrape, which
// may go down as well as up.
type Gauge struct {
	// Name is the metric's name; it comes from a fixed list in the server,
	// never from a request.
	Name string
	// Help says what is measured, as a sentence.
	Help string
	// Value is the value.
	Value float64
}

// Sources are the values a scrape reads from the parts that own them. A nil
// function is left out of the text.
type Sources struct {
	// TenantsByState returns how many tenants are in each state.
	TenantsByState func() map[string]int
	// Counters returns counters kept by other parts, such as the audit
	// writer's and the last-use marks' counts.
	Counters func() []Counter
	// Gauges returns gauges kept by other parts, such as the age of the
	// oldest audit entry not yet sealed.
	Gauges func() []Gauge
}

// histogram is one method's duration histogram: a count per bucket (not
// cumulative; made cumulative when written), the sum and the count.
type histogram struct {
	buckets []uint64
	sum     float64
	count   uint64
}

// requestKey is a method and the code it was answered with.
type requestKey struct{ method, code string }

// Metrics holds the server's own counters and writes the text a scrape
// reads. It is safe for concurrent use.
type Metrics struct {
	sources  Sources
	inFlight atomic.Int64

	mu        sync.Mutex
	requests  map[requestKey]uint64
	durations map[string]*histogram
}

// NewMetrics returns empty metrics that read src at each scrape.
func NewMetrics(src Sources) *Metrics {
	return &Metrics{sources: src, requests: map[requestKey]uint64{}, durations: map[string]*histogram{}}
}

// Observe counts one finished request of method, answered with code, that
// took d. The caller makes sure method and code come from fixed sets, so the
// number of series stays bounded whatever callers send.
func (m *Metrics) Observe(method, code string, d time.Duration) {
	secs := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{method, code}]++
	h := m.durations[method]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(DurationBuckets))}
		m.durations[method] = h
	}
	for i, b := range DurationBuckets {
		if secs <= b {
			h.buckets[i]++
			break
		}
	}
	h.sum += secs
	h.count++
}

// Begin counts a request as in flight; the function it returns ends it.
func (m *Metrics) Begin() (end func()) {
	m.inFlight.Add(1)
	var once sync.Once
	return func() { once.Do(func() { m.inFlight.Add(-1) }) }
}

// Requests returns how many requests of method were answered with code, for
// tests and for the stop report.
func (m *Metrics) Requests(method, code string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests[requestKey{method, code}]
}

// ContentType is the media type of the text WriteText writes.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// WriteText writes every metric in the Prometheus text format.
func (m *Metrics) WriteText(w io.Writer) error {
	b := bufio.NewWriter(w)
	m.writeRequests(b)
	header(b, "nopacs_requests_in_flight", "gauge", "Requests being served now.")
	sample(b, "nopacs_requests_in_flight", nil, strconv.FormatInt(m.inFlight.Load(), 10))
	if f := m.sources.TenantsByState; f != nil {
		header(b, "nopacs_tenants", "gauge", "Tenants by state.")
		states := f()
		for _, s := range slices.Sorted(maps.Keys(states)) {
			sample(b, "nopacs_tenants", []string{"state", s}, strconv.Itoa(states[s]))
		}
	}
	if f := m.sources.Counters; f != nil {
		for _, c := range f() {
			header(b, c.Name, "counter", c.Help)
			sample(b, c.Name, nil, strconv.FormatUint(c.Value, 10))
		}
	}
	if f := m.sources.Gauges; f != nil {
		for _, g := range f() {
			header(b, g.Name, "gauge", g.Help)
			sample(b, g.Name, nil, formatFloat(g.Value))
		}
	}
	return b.Flush()
}

// writeRequests writes the request counter and the duration histogram, in a
// stable order.
func (m *Metrics) writeRequests(b *bufio.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	header(b, "nopacs_requests_total", "counter", "Requests answered, by method and API code.")
	keys := slices.SortedFunc(maps.Keys(m.requests), func(x, y requestKey) int {
		if c := strings.Compare(x.method, y.method); c != 0 {
			return c
		}
		return strings.Compare(x.code, y.code)
	})
	for _, k := range keys {
		sample(b, "nopacs_requests_total", []string{"method", k.method, "code", k.code}, strconv.FormatUint(m.requests[k], 10))
	}
	header(b, "nopacs_request_duration_seconds", "histogram", "Time from a request's arrival to its answer, by method.")
	for _, method := range slices.Sorted(maps.Keys(m.durations)) {
		h := m.durations[method]
		var cum uint64
		for i, bound := range DurationBuckets {
			cum += h.buckets[i]
			sample(b, "nopacs_request_duration_seconds_bucket", []string{"method", method, "le", formatFloat(bound)}, strconv.FormatUint(cum, 10))
		}
		sample(b, "nopacs_request_duration_seconds_bucket", []string{"method", method, "le", "+Inf"}, strconv.FormatUint(h.count, 10))
		sample(b, "nopacs_request_duration_seconds_sum", []string{"method", method}, formatFloat(h.sum))
		sample(b, "nopacs_request_duration_seconds_count", []string{"method", method}, strconv.FormatUint(h.count, 10))
	}
}

// header writes a metric's HELP and TYPE lines.
func header(b *bufio.Writer, name, typ, help string) {
	_, _ = b.WriteString("# HELP " + name + " " + help + "\n# TYPE " + name + " " + typ + "\n")
}

// sample writes one line: the name, the labels given as name and value
// pairs, and the value.
func sample(b *bufio.Writer, name string, labels []string, value string) {
	_, _ = b.WriteString(name)
	if len(labels) > 0 {
		_ = b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				_ = b.WriteByte(',')
			}
			_, _ = b.WriteString(labels[i] + `="` + escapeLabel(labels[i+1]) + `"`)
		}
		_ = b.WriteByte('}')
	}
	_, _ = b.WriteString(" " + value + "\n")
}

// escapeLabel escapes a label value as the text format requires: backslash,
// double quote and line feed.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// formatFloat writes a number the way the text format reads it.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
