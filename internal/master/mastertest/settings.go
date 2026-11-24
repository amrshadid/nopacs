package mastertest

import (
	"fmt"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// testSettings guards each setting operation and the bounds of a name and a
// value.
func testSettings(e *env) {
	_, err := e.s.Setting(e.ctx, "api.limit")
	e.wantKind("Setting, unset", err, fault.NotFound)
	e.ok("SetSetting", e.s.SetSetting(e.ctx, "api.limit", "4 MiB", e.audit(), master.LocalOperator()))
	v, err := e.s.Setting(e.ctx, "api.limit")
	if err != nil || v != "4 MiB" {
		e.Errorf("Setting = %q, %v; want 4 MiB", v, err)
	}
	e.ok("SetSetting again", e.s.SetSetting(e.ctx, "api.limit", "", e.audit(), master.LocalOperator()))
	v, err = e.s.Setting(e.ctx, "api.limit")
	if err != nil || v != "" {
		e.Errorf("Setting = %q, %v; want the empty value, which is a value", v, err)
	}
	big := strings.Repeat("v", master.MaxSettingValueLength-len("é")) + "é"
	e.ok("SetSetting 64 KiB", e.s.SetSetting(e.ctx, "big", big, e.audit(), master.LocalOperator()))
	v, err = e.s.Setting(e.ctx, "big")
	if err != nil || v != big {
		e.Errorf("a 64 KiB value read back with %d bytes, %v", len(v), err)
	}
	e.wantKind("SetSetting, value too big", e.s.SetSetting(e.ctx, "big", big+"x", e.audit(), master.LocalOperator()), fault.Invalid)
	e.wantKind("SetSetting, bad name", e.s.SetSetting(e.ctx, "API Limit", "x", e.audit(), master.LocalOperator()), fault.Invalid)
	removed, err := e.s.UnsetSetting(e.ctx, "api.limit", e.audit(), master.LocalOperator())
	if err != nil || !removed {
		e.Errorf("UnsetSetting = %v, %v; want true", removed, err)
	}
	_, err = e.s.Setting(e.ctx, "api.limit")
	e.wantKind("Setting after UnsetSetting", err, fault.NotFound)
	again := e.audit()
	removed, err = e.s.UnsetSetting(e.ctx, "api.limit", again, master.LocalOperator())
	if err != nil || removed {
		e.Errorf("UnsetSetting of an unset name = %v, %v; want false and success", removed, err)
	}
	if !e.auditIDs()[again.ID] {
		e.Error("unsetting an unset name wrote no audit entry; the request is still recorded")
	}
	_, err = e.s.UnsetSetting(e.ctx, "Bad Name", e.audit(), master.LocalOperator())
	e.wantKind("UnsetSetting, bad name", err, fault.Invalid)
}

// testSettingsListed guards the settings list: by name, paged across
// inserts.
func testSettingsListed(e *env) {
	for i := range 30 {
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("m.%02d", i), "v", e.audit(), master.LocalOperator()))
	}
	n := 0
	checkPaging(e, "ListSettings", func(p master.Page) ([]master.Setting, string, error) { return e.s.ListSettings(e.ctx, p) },
		func(s master.Setting) string { return s.Name },
		func() {
			n++
			e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("a.%02d", n), "v", e.audit(), master.LocalOperator()))
			e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("z.%02d", n), "v", e.audit(), master.LocalOperator()))
		},
		30, 4)
}
