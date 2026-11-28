package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/settings"
)

// settingForms says, for each setting, what a value looks like and what it
// does. The catalog holds the name, the default and who may set it; the forms
// are prose the catalog has no field for. renderSettings refuses a catalog
// setting with no text here, and a text here for no setting, so a setting
// cannot be added or removed without this page being looked at.
var settingForms = map[string]string{
	"access.allowed-networks":        "Networks separated by commas, each an address or `address/bits`. A request from outside them is refused. Empty: every network.",
	"access.block-time":              "A period. How long an address or key that failed twice the limit is blocked.",
	"access.denied-networks":         "Networks separated by commas, each an address or `address/bits`. A request from one is refused before its key is read.",
	"access.failure-limit":           "Failed calls an address or a key may make in the window before its refusals are held one second; at twice as many it is blocked.",
	"access.failure-window":          "A period. The window the failure limit counts in.",
	"access.forwarded-header":        "`x-forwarded-for` or `forwarded` (RFC 7239): the one header a trusted proxy's client address is read from.",
	"access.tenant-networks":         "Networks, as above. Every key used on the tenant must come from one. Empty: every network.",
	"access.trusted-proxies":         "Networks, as above. From one of them, the client named in the forwarded header is the caller. Empty: no forwarded address is believed.",
	"api.cors-origins":               "Origins, one per line, as a browser writes them (`https://app.example.org`), or `*` alone for any. Empty: no page in a browser may read an answer.",
	"audit.retention":                "`0`, or a period. Whole sealed checkpoints older than it lose their entries at the next pass; every checkpoint stays. Export the log first: what is pruned is gone.",
	"audit.seal-interval":            "A period. How often the server seals every audit log, and once more when it stops.",
	"backup.max-duration":            "A period. The longest a backup over the API may run, read when it starts; past it the backup stops, without its summary. Meanwhile it holds a read transaction open, no audit log is sealed and no export runs.",
	"correction.batch-max":           "A whole number. The most subjects, sessions or scans one batch delete names (`nopacs correction delete-batch`).",
	"deid.change-trial-sessions":     "A whole number from 1 to 200. How many of a receiver's newest sessions a profile version is tried over (`nopacs receiver try-profile`) before the receiver is pointed at it; the trial reads the first file of each of their scans.",
	"deid.list-cache-bytes":          "A size. The most bytes the compiled text lists of stored profile versions are kept in, their automata included; the least recently used are dropped past it and built again when next needed.",
	"deid.memory":                    "A size. The most bytes the server's de-identification work holds in memory at once, across every tenant; work that does not fit waits for room.",
	"deid.pixel-limit":               "A size. The most bytes of a decoded image that de-identification works on; a larger image is quarantined with its bytes kept.",
	"delete.grace":                   "A period. How long a delete's bytes stay on the storage, unserved and undoable, before the server purges them; fixed for each delete when it is made.",
	"events.retention":               "A period. How long a tenant's events are kept.",
	"import.allowed-roots":           "Root names separated by commas, each a name from `import.roots`. The roots a tenant's import receivers may read. Empty: none.",
	"import.max-dir-entries":         "A whole number. The most names one directory read by an import may hold.",
	"import.max-units":               "A whole number. The most import units one tenant works at once.",
	"import.roots":                   "One `name=path` per line. The directories an import may read. Empty: no import can read anything.",
	"import.server-max-units":        "A whole number. The most import units of every tenant together that the server works at once.",
	"incoming.limit":                 "A size, such as `200GiB`. The most bytes a tenant may hold in the incoming area; `0` means no limit of the tenant's own.",
	"job.failures-kept":              "A whole number. How many refused instances (and instances sent with a warning) a job lists; the rest are counted, not listed.",
	"key.default-lifetime":           "A period. How long a key made with no expiry works.",
	"key.expiry-warning":             "A period. A key expiring within it is listed in the status of every tenant and reported in the server's log once a day.",
	"key.max-lifetime":               "A period. No key is made or rotated to live longer.",
	"quarantine.warn-age":            "A period. Past it, the oldest file held in quarantine makes the tenant's status warn.",
	"quarantine.warn-bytes":          "A size; `0` for no warning. Past it, the bytes held in quarantine make the tenant's status warn.",
	"quota.bytes":                    "Empty for no bound, or a size of at least one byte. The most bytes of instance files the tenant may hold on its storages, counted from each file's record to its purge; at it, new files are refused as a temporary failure (DIMSE A700H, STOW-RS 507, an import waits) and what was received is archived.",
	"quota.instances":                "Empty for no bound, or a whole number of at least 1. The most instance files the tenant may hold on its storages, each revision its own file, counted as `quota.bytes` is.",
	"quota.warn":                     "A whole percentage. Past this share of a quota's bound the server writes one `QUOTA` event and the tenant's status warns; looked at every minute.",
	"recall.wait":                    "A period. How long a recall may stay requested or in progress; past it the server records it failed (\"the storage did not bring the object back within recall.wait\") and asks the store nothing more. Read at each pass of the recall worker.",
	"replica.lag-alert":              "A period. When a replica's oldest scan not in step has waited longer, the server writes one `REPLICA_BEHIND` event, and one more (`recovered`) when the replica is back under it. Looked at every minute.",
	"replica.rate":                   "`0` for no bound, or a size, put each second. The most bytes a second the replicator puts on one replica, shared by every copy to it at once; a put's time limit grows with the time the bound makes it take. Read at each claim of scans to copy.",
	"replica.repair":                 "`manual` or `auto`. Whether a primary's object a scrub found damaged or missing is repaired from a replica by a person's `nopacs repair`, or by the server by itself after the scrub.",
	"replica.repair-lease":           "A period. How long a repair's claim holds its unit, which bounds one put of an object. A lease too short to put the largest file a receiver of the tenant accepts at 100 MiB a second is refused, logged, and no repair runs until it is lengthened.",
	"retention.max-clock-step":       "A period. How far forward the server's clock may move between two checks before retention pauses until an admin confirms the time.",
	"retention.max-per-pass":         "A whole number. The most sessions one daily pass of a delete rule disposes of.",
	"retention.shorten-notice":       "A period. A change of rules that would make data due sooner, or keep it less, takes effect no sooner than this after it is made.",
	"read.per-caller":                "A whole number. The most reads of archived bytes one caller (a key, or the local operator) holds open at once; a read past it is refused, never queued.",
	"read.per-server":                "A whole number. The most reads the server holds open at once; it may not be below `read.per-caller`.",
	"receiver.allow-as-received":     "`true` or `false`. Whether a receiver of the tenant may archive what it receives without de-identifying it.",
	"receiver.bind-addresses":        "IP addresses separated by commas; the first is the default. Empty: listen on every address.",
	"receiver.header-limit":          "A size. The most bytes of a file's header; a longer one is quarantined with its bytes kept.",
	"receiver.ports":                 "Ports and ranges separated by commas, such as `11112,11120-11129`. The ports a tenant's DIMSE receivers may use. Empty: no DIMSE receiver.",
	"scrub.archive-sample":           "A whole percentage. The share of the objects online on a storage of the archive tier whose bytes a scrub reads; the rest, and every object not online, are checked by presence only (size, and the store's own SHA-256 where it keeps one). A scrub never asks for a recall. Read at the start of each unit of a scrub.",
	"scrub.interval":                 "`0` for never, or a period. How often the server checks each of a tenant's storages by itself.",
	"scrub.rate":                     "A size, read each second. The most bytes a second a scrub reads from one storage.",
	"search.facet-rows":              "A whole number of rows. The most rows a search counts to answer how many match, or reads to give a facet: a count of a search that matches more answers \"at least\" this many, and a facet of one is refused. Narrow the search, or raise the bound at the cost of a slower count and facet.",
	"sender.allow-plain-http":        "`true` or `false`. Whether a STOW-RS sender's URL may be plain http.",
	"sender.destinations":            "One line per destination: a host name, an address or a range, then optionally `ports=...` and the flags `private-address` and `link-local`. Empty: no sender can be added. The long form is on the settings page.",
	"deid.base-profile":              "Empty for none, or a profile version of the tenant as `name@version`, such as `research@2`. The tenant's base: the profile of every receiver that de-identifies, of every archive rule and of every destination of a rule set must be at least as strict as it (a rule that keeps what the base removes is refused with the loosening listed), and a reception runs the receiver's profile stacked over it. Refused to a masked key. `nopacs profile describe --effective` shows each element's winning layer.",
	"sender.max-associations":        "A whole number. The most associations (STOW-RS requests) all senders of all tenants hold open at once.",
	"sender.tenant-max-associations": "A whole number. The most associations one tenant's senders hold open at once.",
	"storage.directory-roots":        "One `name=path` per line. The roots directory storages may lie under. Empty: no directory storage.",
	"storage.reserve":                "One or two sizes or percentages, comma-separated, such as `10%,10GiB`; the larger applies. The free space kept on every disk or store data is written to.",
	"storage.s3-endpoints":           "One URL per line, optionally followed by `plain-http` and `private-address`. Empty: no S3 storage.",
	"transfer.wait":                  "A period. How long a transfer between tenants may wait on refusals that pass by themselves (the target's receiver busy or over its quota, a tenant not ready); past it the transfer's job ends failed and the subject is free again.",
}

// settingBounds holds, for a setting with a range, four values: one just
// below the least it accepts, the least, the most, and one just above. The
// page prints the two inside values; the test checks all four against the
// setting's own rule, so the page cannot print a bound the program does not
// hold. An empty first or last value means there is nothing below or above.
var settingBounds = map[string][4]string{
	"access.block-time":              {"0s", "1s", "7d", "8d"},
	"access.failure-limit":           {"0", "1", "10000", "10001"},
	"access.failure-window":          {"0s", "1s", "24h", "25h"},
	"audit.retention":                {"89d", "90d", "36500d", "36501d"},
	"audit.seal-interval":            {"59s", "1m", "1d", "25h"},
	"backup.max-duration":            {"59s", "1m", "7d", "8d"},
	"correction.batch-max":           {"0", "1", "10000", "10001"},
	"deid.change-trial-sessions":     {"0", "1", "200", "201"},
	"deid.list-cache-bytes":          {"63MiB", "64MiB", "16GiB", "17GiB"},
	"deid.memory":                    {"255MiB", "256MiB", "64GiB", "65GiB"},
	"deid.pixel-limit":               {"1023KiB", "1MiB", "64GiB", "65GiB"},
	"delete.grace":                   {"", "0d", "90d", "91d"},
	"events.retention":               {"23h", "1d", "3650d", "3651d"},
	"import.max-dir-entries":         {"999", "1000", "10000000", "10000001"},
	"import.max-units":               {"0", "1", "64", "65"},
	"import.server-max-units":        {"0", "1", "256", "257"},
	"job.failures-kept":              {"99", "100", "1000000", "1000001"},
	"key.default-lifetime":           {"59m", "1h", "3650d", "3651d"},
	"key.expiry-warning":             {"59m", "1h", "365d", "366d"},
	"key.max-lifetime":               {"59m", "1h", "3650d", "3651d"},
	"quarantine.warn-age":            {"23h", "1d", "3650d", "3651d"},
	"read.per-caller":                {"0", "1", "1024", "1025"},
	"read.per-server":                {"15", "16", "65536", "65537"},
	"receiver.header-limit":          {"1023KiB", "1MiB", "4GiB", "5GiB"},
	"recall.wait":                    {"59m", "1h", "30d", "31d"},
	"replica.lag-alert":              {"59s", "1m", "30d", "31d"},
	"replica.rate":                   {"1023KiB", "1MiB", "10GiB", "11GiB"},
	"replica.repair-lease":           {"29s", "30s", "1h", "61m"},
	"retention.max-clock-step":       {"59m", "1h", "30d", "31d"},
	"quota.instances":                {"0", "1", "4611686018427387904", "4611686018427387905"},
	"quota.warn":                     {"0%", "1%", "100%", "101%"},
	"retention.max-per-pass":         {"0", "1", "100000", "100001"},
	"retention.shorten-notice":       {"23h", "1d", "365d", "366d"},
	"scrub.archive-sample":           {"", "0%", "100%", "101%"},
	"scrub.interval":                 {"23h", "1d", "3650d", "3651d"},
	"scrub.rate":                     {"1023KiB", "1MiB", "64GiB", "65GiB"},
	"search.facet-rows":              {"999", "1000", "100000000", "100000001"},
	"sender.max-associations":        {"0", "1", "1024", "1025"},
	"sender.tenant-max-associations": {"0", "1", "1024", "1025"},
	"transfer.wait":                  {"59m", "1h", "30d", "31d"},
}

// scopeOf says where a setting may hold a value.
func scopeOf(d settings.Definition) string {
	switch {
	case d.TenantOnly:
		return "each tenant; no installation value"
	case d.TenantOverride && d.TenantSetByInstallation:
		return "the installation (every tenant's default) and each tenant"
	case d.TenantOverride:
		return "the installation, which each tenant may override"
	}
	return "the installation only"
}

// whoOf says who may set a setting.
func whoOf(d settings.Definition) string {
	if d.TenantOnly && !d.TenantSetByInstallation {
		return "the tenant's admin"
	}
	if d.TenantOverride && !d.TenantSetByInstallation {
		return "admin on the installation; the tenant's admin for the tenant's value"
	}
	return "admin on the installation"
}

// renderSettings writes the settings table from the built-in catalog.
func renderSettings(string) (string, error) {
	defs := settings.Builtin()
	slices.SortFunc(defs, func(a, b settings.Definition) int { return strings.Compare(a.Name, b.Name) })
	known := map[string]bool{}
	var b strings.Builder
	b.WriteString("Every setting the server knows, with its default, where it may hold a value, who may set it and\n" +
		"whether a change needs a restart. A setting is read with `nopacs setting get` or `list` and changed\n" +
		"with `nopacs setting set` and `unset` (`--tenant NAME` for a tenant's value). A period is a whole number\n" +
		"followed by `s`, `m`, `h` or `d`; a size is a whole number of bytes, or a number followed by `KiB`, `MiB`,\n" +
		"`GiB`, `TiB` or `PiB`.\n\n" +
		"None of these needs a restart: each is read from the database when it is used, or kept for a few seconds.\n" +
		"When a change is seen, setting by setting, is on [the settings page](../operate/settings.md#which-changes-take-effect-without-a-restart).\n" +
		"The server's own flags and files (`--listen`, the key file, the data directory) are read once, at start.\n\n")
	b.WriteString("| Setting | Default | Holds a value for | Who may set it | Range | What it is | Restart |\n|---|---|---|---|---|---|---|\n")
	for _, d := range defs {
		known[d.Name] = true
		form, ok := settingForms[d.Name]
		if !ok {
			return "", fmt.Errorf("the setting %s has no text in settingForms", d.Name)
		}
		def := "empty"
		if d.Default != "" {
			def = "`" + d.Default + "`"
		}
		rng := "-"
		if r, ok := settingBounds[d.Name]; ok {
			rng = "`" + r[1] + "` to `" + r[2] + "`"
			if d.Name == "audit.retention" || d.Name == "scrub.interval" || d.Name == "replica.rate" {
				rng = "`0`, or " + rng
			}
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | no |\n", d.Name, def, cell(scopeOf(d)), cell(whoOf(d)), rng, cell(form))
	}
	for name := range settingForms {
		if !known[name] {
			return "", fmt.Errorf("settingForms has text for %s, which the catalog does not hold", name)
		}
	}
	for name := range settingBounds {
		if !known[name] {
			return "", fmt.Errorf("settingBounds has a range for %s, which the catalog does not hold", name)
		}
	}
	return b.String(), nil
}
