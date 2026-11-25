//go:build clock

package clock

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fileEvery is how long a read of the offset file is kept.
const fileEvery = 200 * time.Millisecond

// source is where the offset comes from: a fixed one, or a file read again
// every `every`. The fields are unexported and set only here and by this
// package's tests.
type source struct {
	fixed time.Duration
	path  string
	every time.Duration

	mu   sync.Mutex
	read time.Time
	file time.Duration
	have bool
}

// current is the process's source, made when the package is initialized from
// the environment: a package variable set here and never by a test, as the
// step a tagged build stops at is (internal/interrupt).
var current = load(os.Getenv(EnvOffset), os.Getenv(EnvFile))

// load makes the source of the variables' values and ends the process with
// status 2 on an offset it cannot read.
func load(offset, path string) *source {
	s := &source{path: path, every: fileEvery}
	if offset != "" {
		d, err := parse(offset)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "clock: %s: %v\n", EnvOffset, err)
			os.Exit(2)
		}
		s.fixed = d
	}
	return s
}

// parse reads an offset: a Go duration ("36h", "-90m") or a number of days
// ("400d", "0.5d").
func parse(text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n > 36500 || n < -36500 {
			return 0, fmt.Errorf("%q is not a number of days between -36500 and 36500", text)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("%q is not an offset such as 36h or 400d", text)
	}
	return d, nil
}

// offset returns the offset now: the file's once it exists, else the fixed one.
func (s *source) offset(at time.Time) time.Duration {
	if s.path == "" {
		return s.fixed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.have && at.Sub(s.read) < s.every {
		return s.file
	}
	s.read = at
	text, err := os.ReadFile(s.path)
	switch {
	case os.IsNotExist(err):
		s.file, s.have = s.fixed, true
	case err != nil:
		_, _ = fmt.Fprintf(os.Stderr, "clock: %s: %v\n", EnvFile, err)
		os.Exit(2)
	default:
		d, perr := parse(string(text))
		if perr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "clock: %s: %v\n", EnvFile, perr)
			os.Exit(2)
		}
		s.file, s.have = d, true
	}
	return s.file
}

// now returns the system's time moved by the offset.
func (s *source) now() time.Time {
	at := time.Now()
	return at.Add(s.offset(at))
}

// Now returns the system's time plus the offset the environment gave.
func Now() time.Time { return current.now() }
