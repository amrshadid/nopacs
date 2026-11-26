package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newKeyList makes the list of keys.
func newKeyList(c *ctx) (screen, tea.Cmd) {
	l := newList(c, "Keys", []column{{"ID", 12, 14}, {"HOLDER", 6, 40}, {"STATUS", 6, 8}, {"CREATED", 10, 10}, {"EXPIRES", 7, 10}, {"LAST USED", 7, 10}},
		"no key yet; n makes one")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return keysPage(c, l.id, l.gen, token) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		k, _ := r.ref.(*v1.Key)
		s, cmd := newKeyDetail(c, k, false)
		return push(s, cmd)
	}
	l.actions = []action{
		{key: "n", help: "new key", do: func(c *ctx, _ screen, _ any) tea.Cmd {
			s, cmd := newNewKey(c)
			return push(s, cmd)
		}},
		{key: "r", help: "revoke", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			k, _ := ref.(*v1.Key)
			s, cmd := newRevokeKey(c, k, 1)
			return push(s, cmd)
		}},
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newKeyDetail makes the screen of one key, with its grants one key away.
func newKeyDetail(c *ctx, k *v1.Key, fetch bool) (screen, tea.Cmd) {
	d := detailScreen{base: base{c.newID()}, heading: "Key " + clean(k.GetId()), ref: k}
	d.pairs = func(c *ctx, ref any) []pair {
		k, _ := ref.(*v1.Key)
		never := func(ts *timestamppb.Timestamp, absent string) string {
			if ts == nil {
				return absent
			}
			return output.Time(ts, c.loc)
		}
		last := never(k.GetLastUseTime(), "never")
		if addr := k.GetLastUseAddress(); addr != "" && k.GetLastUseTime() != nil {
			last += " from " + addr
		}
		ps := []pair{
			{"ID", k.GetId()},
			{"Holder", k.GetHolder()},
			{"Status", output.KeyStatus(k, c.now())},
			{"Created", never(k.GetCreateTime(), "-")},
			{"Expires", never(k.GetExpireTime(), "never")},
			{"Last used", last},
			{"Revoked", never(k.GetRevokeTime(), "-")},
		}
		if left, soon := keyExpiresSoon(k, c.now()); soon {
			ps = append(ps, pair{"Warning", "expires in " + left + "; make its successor with \"nopacs key rotate\" before then"})
		}
		if n := k.GetNetworks(); len(n) > 0 {
			ps = append(ps, pair{"Networks", strings.Join(n, ", ")})
		}
		if cert := k.GetClientCertSha256(); cert != "" {
			ps = append(ps, pair{"Client cert", "sha256 " + cert})
		}
		if succ := k.GetSuccessorId(); succ != "" {
			ps = append(ps, pair{"Successor", succ})
		}
		return ps
	}
	d.refetch = func(c *ctx, id int, ref any) tea.Cmd {
		k, _ := ref.(*v1.Key)
		return getKey(c, id, k.GetId())
	}
	d.actions = []action{
		{key: "g", help: "grants", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			k, _ := ref.(*v1.Key)
			s, cmd := newGrantList(c, grantFilter{keyID: k.GetId()})
			return push(s, cmd)
		}},
		{key: "r", help: "revoke", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			k, _ := ref.(*v1.Key)
			s, cmd := newRevokeKey(c, k, 1)
			return push(s, cmd)
		}},
	}
	if fetch {
		d.loading = true
		return d, d.refetch(c, d.id, k)
	}
	return d, nil
}

// newNewKey makes the form that asks for a holder and an expiry. The key
// itself is not here: the answer replaces this form with the screen that shows
// it, so going back never finds a form with a holder typed in and nothing to
// show for it.
func newNewKey(c *ctx) (screen, tea.Cmd) {
	f := formScreen{
		base:    base{c.newID()},
		heading: "New key",
		intro:   []string{"A key is a credential for a person or a system. It has no rights until it is given a grant."},
		fields: []formField{
			{label: "Holder", hint: "who holds the key: a person or a system"},
			{label: "Expires", hint: "after 90d, 12h, 2w ... or never", in: field{}.set("90d")},
		},
		verb: "make the key",
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		holder := f.value(0)
		if holder == "" || len(holder) > 256 {
			return nil, fault.New(fault.Invalid, "the holder is required and at most 256 bytes")
		}
		var expire time.Duration
		if e := f.value(1); !strings.EqualFold(e, "never") {
			d, err := output.ParseSpan(e)
			if err != nil {
				return nil, fault.New(fault.Invalid, "the expiry is not a length of time; use a number and s, m, h, d or w, such as 90d, or the word never")
			}
			expire = d
		}
		return createKey(c, f.id, holder, expire), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		made, ok := msg.(keyCreatedMsg)
		if !ok {
			return f, nil, false
		}
		s := newSecretScreen(c, made.key, made.secret)
		return f, func() tea.Msg { return replaceMsg{s: s} }, true
	}
	return f, nil
}

// newRevokeKey asks for the key's id to be typed before it is revoked.
func newRevokeKey(c *ctx, k *v1.Key, pop int) (screen, tea.Cmd) {
	id := k.GetId()
	f := formScreen{
		base:    base{c.newID()},
		heading: "Revoke key " + clean(id),
		intro: []string{
			"Revoke key " + output.KeyLabel(k) + ".",
			"From this moment a request with it is refused. There is no way to undo it; make a new key instead.",
		},
		fields:   []formField{{label: "Type the key id", hint: id}},
		want:     id,
		confirms: true,
		verb:     "revoke",
		pop:      pop,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) { return revokeKey(c, f.id, id), nil }
	return f, nil
}

// keyWarnWithin is how long before its expiry a key is shown as about to
// expire: the built-in value of the installation's key.expiry-warning, which
// a client cannot read without more rights than a key lister has.
const keyWarnWithin = 7 * 24 * time.Hour

// keyExpiresSoon says whether a key that works now expires within the
// warning, and in how long, in words.
func keyExpiresSoon(k *v1.Key, now time.Time) (string, bool) {
	if k.GetRevokeTime() != nil || k.GetExpireTime() == nil {
		return "", false
	}
	left := k.GetExpireTime().AsTime().Sub(now)
	if left <= 0 || left > keyWarnWithin {
		return "", false
	}
	if left < 48*time.Hour {
		return fmt.Sprintf("%d hours", int(left.Hours())), true
	}
	return fmt.Sprintf("%d days", int(left.Hours()/24)), true
}

// keyStatusWord is a key's status for a list: expiring while it works but is
// about to stop.
func keyStatusWord(k *v1.Key, now time.Time) string {
	st := output.KeyStatus(k, now)
	if _, soon := keyExpiresSoon(k, now); soon && st == "active" {
		return "expiring"
	}
	return st
}
