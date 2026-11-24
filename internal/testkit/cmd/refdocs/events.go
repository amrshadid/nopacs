package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// enumComments reads the comment above each value of the enum named enum in a
// proto file's text. The generated Go code keeps the values but not their
// comments, and the contract's comment is the one place that says what an
// event type is, so the page reads it from there rather than say it twice.
func enumComments(proto, enum string) (map[string]string, error) {
	start := regexp.MustCompile(`^enum ` + regexp.QuoteMeta(enum) + ` \{`)
	value := regexp.MustCompile(`^\s+([A-Z][A-Z0-9_]*) = \d+;`)
	out := map[string]string{}
	var comment []string
	inside := false
	sc := bufio.NewScanner(strings.NewReader(proto))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case !inside:
			inside = start.MatchString(line)
		case strings.HasPrefix(line, "}"):
			return out, nil
		case strings.HasPrefix(strings.TrimSpace(line), "//"):
			comment = append(comment, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "//")))
		default:
			if m := value.FindStringSubmatch(line); m != nil {
				out[m[1]] = strings.Join(comment, " ")
			}
			comment = nil
		}
	}
	return nil, fmt.Errorf("the enum %s is not in the proto file", enum)
}

// renderEvents lists the event types of the contract: the generated enum's
// values, each with the contract's comment.
func renderEvents(root string) (string, error) {
	text, err := os.ReadFile(filepath.Join(root, "api", "proto", "nopacs", "v1", "monitor.proto"))
	if err != nil {
		return "", err
	}
	comments, err := enumComments(string(text), "EventType")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("A tenant's events are its live feed: `nopacs watch <tenant>` and `MonitorService.Watch` follow them, and\n" +
		"`--type` (`types` in the call) keeps only the types named. An event is written in the same transaction as the\n" +
		"change it reports, and is kept for the tenant's `events.retention`. A client skips an event of a type it does\n" +
		"not know and takes its cursor, so a type added in a later version breaks no client.\n\n" +
		"| Event type | Number | What it reports |\n|---|---|---|\n")
	for n := int32(0); int(n) < len(v1.EventType_name); n++ {
		name, ok := v1.EventType_name[n]
		if !ok {
			return "", fmt.Errorf("the event type numbers are not consecutive at %d", n)
		}
		if n == 0 {
			continue // Never answered by the server.
		}
		text, ok := comments[name]
		if !ok || text == "" {
			return "", fmt.Errorf("%s has no comment in monitor.proto", name)
		}
		fmt.Fprintf(&b, "| `%s` | %d | %s |\n", strings.TrimPrefix(name, "EVENT_TYPE_"), n, cell(text))
	}
	return b.String(), nil
}
