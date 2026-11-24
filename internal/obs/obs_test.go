package obs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// The secrets the logging tests plant.
const (
	plantedKey     = "nopacs_LOGTESTKEY1_LOGSECRETLOGSECRETLOGSECRETLOGSECRETLOGSECRET1"
	plantedAddress = "postgres://owner:s3cret-pw@db.example/lab"                          //nolint:gosec // G101: a planted test secret.
	plantedBearer  = "Bearer nopacs_HEADERKEY01_HEADERSECRETHEADERSECRETHEADERSECRETHEAD" //nolint:gosec // G101: a planted test secret.
)

// logEverything logs both secret-carrying messages and an authorization
// value at debug level, in every way a careless caller could, and returns
// what was written.
func logEverything(format Format, redactMessages bool) string {
	var buf bytes.Buffer
	log := newLogger(&buf, LogOptions{Format: format, Level: slog.LevelDebug}, redactMessages)
	req := &nopacsv1.CreateTenantRequest{Name: "lab", DatabaseType: nopacsv1.DatabaseType_DATABASE_TYPE_POSTGRES, DatabaseAddress: plantedAddress}
	res := &nopacsv1.CreateKeyResponse{Key: &nopacsv1.Key{Id: "LOGTESTKEY1", Holder: "robot"}, Secret: plantedKey}
	log.Debug("request", "message", req)
	log.Debug("response", slog.Any("message", res))
	log.DebugContext(context.Background(), "grouped", slog.Group("call", slog.Any("in", req), slog.Any("out", res)))
	log.With("message", res).Debug("with")
	log.Debug("header", "authorization", plantedBearer, slog.Group("headers", slog.String("Authorization", plantedBearer)))
	return buf.String()
}

// TestLoggerRedacts guards the redaction of secrets for both slog handlers,
// since the protobuf library itself prints a debug_redact field: the two
// secret fields and an authorization value, logged at debug level, are absent
// from the output, and the lines are there with their other fields.
func TestLoggerRedacts(t *testing.T) {
	for _, f := range []Format{JSON, Text} {
		out := logEverything(f, true)
		for _, s := range []string{plantedKey, plantedAddress, "s3cret-pw", plantedBearer, "HEADERSECRET"} {
			if strings.Contains(out, s) {
				t.Fatalf("%s handler wrote a secret:\n%s", f, out)
			}
		}
		for _, s := range []string{"robot", "lab", HiddenValue, "grouped", "with"} {
			if !strings.Contains(out, s) {
				t.Fatalf("%s handler lost %q:\n%s", f, s, out)
			}
		}
	}
}

// TestLoggerWithoutRedactorLeaks shows that TestLoggerRedacts would fail
// without the redactor: with it turned off, the same lines carry both
// secrets. It guards the test itself from passing for the wrong reason.
func TestLoggerWithoutRedactorLeaks(t *testing.T) {
	for _, f := range []Format{JSON, Text} {
		out := logEverything(f, false)
		if !strings.Contains(out, plantedKey) || !strings.Contains(out, "s3cret-pw") {
			t.Fatalf("%s handler: without the redactor the secrets should be in the output:\n%s", f, out)
		}
	}
}

// TestParseLevel guards the names the serve command accepts for --log-level.
func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, ok := ParseLevel(in); !ok || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, ok)
		}
	}
	if _, ok := ParseLevel("loud"); ok {
		t.Error("an unknown level was accepted")
	}
}

// TestMetricsText guards the text format: counters, the cumulative
// histogram with its +Inf bucket, sum and count, the gauges read from their
// sources, label escaping, and HELP and TYPE lines for each family.
func TestMetricsText(t *testing.T) {
	m := NewMetrics(Sources{
		TenantsByState: func() map[string]int { return map[string]int{"ready": 2, "failed": 1} },
		Counters: func() []Counter {
			return []Counter{
				{Name: "nopacs_audit_entries_written_total", Help: "Written.", Value: 7},
				{Name: "nopacs_audit_entries_dropped_refusals_total", Help: "Dropped.", Value: 1},
				{Name: "nopacs_key_last_use_marks_written_total", Help: "Marks.", Value: 3},
			}
		},
	})
	m.Observe("/nopacs.v1.AccessService/WhoAmI", "ok", 2*time.Millisecond)
	m.Observe("/nopacs.v1.AccessService/WhoAmI", "unauthenticated", 40*time.Second)
	m.Observe(`we"ird\`, "ok", time.Microsecond)
	end := m.Begin()
	var buf bytes.Buffer
	if err := m.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	end()
	end()
	out := buf.String()
	for _, want := range []string{
		"# TYPE nopacs_requests_total counter\n",
		`nopacs_requests_total{method="/nopacs.v1.AccessService/WhoAmI",code="ok"} 1` + "\n",
		`nopacs_request_duration_seconds_bucket{method="/nopacs.v1.AccessService/WhoAmI",le="0.0025"} 1` + "\n",
		`nopacs_request_duration_seconds_bucket{method="/nopacs.v1.AccessService/WhoAmI",le="30"} 1` + "\n",
		`nopacs_request_duration_seconds_bucket{method="/nopacs.v1.AccessService/WhoAmI",le="+Inf"} 2` + "\n",
		`nopacs_request_duration_seconds_count{method="/nopacs.v1.AccessService/WhoAmI"} 2` + "\n",
		`method="we\"ird\\"`,
		"nopacs_requests_in_flight 1\n",
		`nopacs_tenants{state="failed"} 1` + "\n",
		"nopacs_audit_entries_written_total 7\n",
		"# TYPE nopacs_audit_entries_dropped_refusals_total counter\n",
		"nopacs_audit_entries_dropped_refusals_total 1\n",
		"nopacs_key_last_use_marks_written_total 3\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if m.inFlight.Load() != 0 {
		t.Error("an ended request is still in flight, or ending twice counted twice")
	}
}
