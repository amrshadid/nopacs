package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestSecretsNeverPrint guards that no way of printing or encoding a Store, a
// Config, or a struct that holds either shows the access key id or the
// secret: every fmt verb, Sprint, both slog handlers, encoding/json, a
// Config in an unexported field (which fmt prints field by field without
// calling methods), and an error that wraps them.
func TestSecretsNeverPrint(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	cfg := f.config()
	type holder struct {
		cfg Config
		set settings
	}
	type exported struct {
		Cfg  Config
		Auth Secret
	}
	h := holder{cfg, s.set}
	e := exported{cfg, cfg.SecretAccessKey}
	values := map[string]any{
		"store": s, "config": cfg, "&config": &cfg, "settings": s.set,
		"secret": cfg.SecretAccessKey, "&secret": &cfg.SecretAccessKey, "holder": h, "&holder": &h,
		"exported": e, "slice": []any{cfg, s, cfg.AccessKeyID}, "map": map[string]any{"k": cfg},
	}
	bad := []string{fakeSecret, fakeKey}
	check := func(what, got string) {
		t.Helper()
		for _, b := range bad {
			if strings.Contains(got, b) {
				t.Errorf("%s shows %q", what, b)
			}
		}
	}
	for name, v := range values {
		verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%T %v", "%10v", "%-20s"}
		if strings.Contains(name, "store") || name == "slice" {
			// A verb that does not fit a pointer (%s, %q) makes fmt print what
			// the pointer points to, field by field; for a *Store that is the
			// HTTP transport's connection pool, which other goroutines are
			// changing, and the race detector rightly objects to the walk.
			verbs = verbs[:3]
		}
		for _, verb := range verbs {
			check(name+" with "+verb, fmt.Sprintf(verb, v))
		}
		check(name+" Sprint", fmt.Sprint(v))
		check(name+" Sprintln", fmt.Sprintln(v))
		check(name+" wrapped in an error", fmt.Errorf("opening: %w (%v)", fmt.Errorf("inner %+v", v), v).Error())
		if b, err := json.Marshal(v); err == nil {
			check(name+" json", string(b))
		}
		if b, err := json.MarshalIndent(v, "", " "); err == nil {
			check(name+" json indent", string(b))
		}
		for kind, mk := range map[string]func(*bytes.Buffer) slog.Handler{
			"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
			"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		} {
			var buf bytes.Buffer
			l := slog.New(mk(&buf))
			l.Info("opened", "value", v)
			l.LogAttrs(context.Background(), slog.LevelInfo, "again", slog.Any("v", v))
			l.With("v", v).Info("with")
			check(name+" slog "+kind, buf.String())
		}
	}
	// What a Secret is for still works.
	if cfg.SecretAccessKey.Reveal() != fakeSecret || cfg.AccessKeyID.Reveal() != fakeKey {
		t.Error("Reveal does not give the value")
	}
	var u Secret
	if err := u.UnmarshalText([]byte("abc")); err != nil || u.Reveal() != "abc" || fmt.Sprint(u) == "abc" {
		t.Errorf("UnmarshalText: %q, %v", u.Reveal(), err)
	}
}
