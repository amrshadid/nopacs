package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/servecmd"
)

// metricLabels names the labels of the metrics that have any. A metric's
// labels show only in its samples, and a server that has just started has
// samples for few of them, so the names are written here; the page refuses a
// sample that carries a label this table does not list.
var metricLabels = map[string][]string{
	"nopacs_requests_total":           {"method", "code"},
	"nopacs_request_duration_seconds": {"method"},
	"nopacs_tenants":                  {"state"},
}

// metric is one family of the text a scrape returns.
type metric struct {
	name, typ, help string
	labels          map[string]bool
}

// renderMetrics starts a real server on a throwaway installation, scrapes its
// metrics twice (a request is counted after it is answered, so the second
// scrape has the first one's) and writes what it exposes. What the server
// exposes is then what the page says, whatever part of the server added it.
func renderMetrics(string) (string, error) {
	text, err := scrapeServer()
	if err != nil {
		return "", err
	}
	families, err := parseMetrics(text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("What `GET /metrics` returns, in the Prometheus text format (version 0.0.4). The server answers it on its local\n" +
		"socket with no key; on the network it needs a key that holds a grant on the installation. A scrape writes no audit\n" +
		"entry. Every name begins `nopacs_`; a label's values come from fixed sets of the server, never from a request, so the\n" +
		"number of series stays bounded whatever callers send.\n\n" +
		"| Metric | Type | Labels | What it counts or measures |\n|---|---|---|---|\n")
	for _, m := range families {
		labels := slices.Sorted(mapsKeys(m.labels))
		cellLabels := "-"
		if len(labels) > 0 {
			cellLabels = "`" + strings.Join(labels, "`, `") + "`"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", m.name, m.typ, cellLabels, cell(m.help))
	}
	fmt.Fprintf(&b, "\nThe buckets of `nopacs_request_duration_seconds`, in seconds, are fixed so a dashboard built on them keeps\nworking: %s.\n", joinFloats(obs.DurationBuckets))
	return b.String(), nil
}

// mapsKeys returns the keys of a set as a sequence for slices.Sorted.
func mapsKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// joinFloats writes bounds as a list.
func joinFloats(v []float64) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return strings.Join(parts, ", ")
}

// parseMetrics reads the # HELP and # TYPE lines and the samples of a scrape.
func parseMetrics(text string) ([]*metric, error) {
	byName := map[string]*metric{}
	var order []*metric
	family := func(name string) *metric {
		if m, ok := byName[name]; ok {
			return m
		}
		m := &metric{name: name, labels: map[string]bool{}}
		byName[name] = m
		order = append(order, m)
		return m
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, help, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			family(name).help = help
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			family(name).typ = typ
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			name, rest, hasLabels := strings.Cut(line, "{")
			if !hasLabels {
				continue
			}
			name = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
			_, ok := byName[name]
			if !ok {
				continue
			}
			inside, _, _ := strings.Cut(rest, "}")
			for _, pair := range strings.Split(inside, ",") {
				k, _, _ := strings.Cut(pair, "=")
				if k == "le" {
					continue
				}
				if !slices.Contains(metricLabels[name], k) {
					return nil, fmt.Errorf("%s has a sample with the label %q, which metricLabels does not list", name, k)
				}
			}
		}
	}
	for _, m := range order {
		if m.typ == "" || m.help == "" {
			return nil, fmt.Errorf("the metric %s has no HELP or no TYPE line", m.name)
		}
		for _, l := range metricLabels[m.name] {
			m.labels[l] = true
		}
	}
	return order, nil
}

// scrapeServer makes an installation in a short temporary path (a local socket's
// path is limited to about a hundred bytes), serves it in a child process of
// this program, scrapes /metrics twice and stops the child.
func scrapeServer() (string, error) {
	dir, err := os.MkdirTemp("/tmp", "refdocs")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if code := servecmd.Run(ctx, []string{"init", "--data-dir", dir}, servecmd.Env{Stdout: io.Discard, Stderr: io.Discard, Getenv: os.Getenv}); code != 0 {
		return "", fmt.Errorf("init of the throwaway installation exited %d", code)
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	child := exec.CommandContext(ctx, self, "-serve-child", dir) //nolint:gosec // G204: this program's own path.
	if err := child.Start(); err != nil {
		return "", err
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", filepath.Join(dir, "run", "nopacs.sock"))
	}}}
	defer hc.CloseIdleConnections()
	var text string
	var last error
	for scrape := 0; scrape < 2; {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("the throwaway server gave no metrics: %w", errors.Join(ctx.Err(), last))
		default:
		}
		resp, err := hc.Get("http://local/metrics")
		if err != nil {
			last = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			last = err
			if err == nil {
				last = fmt.Errorf("the scrape was answered with status %d", resp.StatusCode)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		text = string(body)
		scrape++
	}
	if err := child.Process.Signal(os.Interrupt); err != nil {
		return "", err
	}
	stopped = true
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = child.Process.Kill()
		<-done
	}
	return text, nil
}

// serveForMetrics is the child's side: it runs the real server of the data
// directory until it is interrupted.
func serveForMetrics(dir string) int {
	return servecmd.Run(context.Background(), []string{"serve", "--data-dir", dir}, servecmd.Env{Stdout: io.Discard, Stderr: io.Discard, Getenv: os.Getenv})
}
