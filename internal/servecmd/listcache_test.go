package servecmd

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestListCacheBoundFollowsTheSetting guards the wiring of
// deid.list-cache-bytes: starting sets the text lists' cache bound to the
// value read; a value that cannot be read keeps the bound as it was; and
// stopping ends the timer.
func TestListCacheBoundFollowsTheSetting(t *testing.T) {
	t.Cleanup(func() { profile.SetListCacheBytes(profile.DefaultListCacheBytes) })
	b := &listCacheBound{read: func(context.Context) (int64, error) { return 128 << 20, nil }, log: slog.New(slog.DiscardHandler)}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, bound, _ := profile.ListCacheBytes(); bound != 128<<20 {
		t.Errorf("the bound is %d after start, want 128 MiB", bound)
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	failing := &listCacheBound{read: func(context.Context) (int64, error) { return 0, errors.New("no master") }, log: slog.New(slog.DiscardHandler)}
	failing.apply(context.Background())
	if _, bound, _ := profile.ListCacheBytes(); bound != 128<<20 {
		t.Errorf("an unreadable setting changed the bound to %d", bound)
	}
}
