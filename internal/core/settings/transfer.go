package settings

import "time"

// TransferWait is how long a transfer between tenants may wait on refusals
// that pass by themselves (the target's receiver busy or over its quota, a
// tenant not ready) before its job ends failed (ParseTransferWait).
const TransferWait = "transfer.wait"

// MinTransferWait and MaxTransferWait bound transfer.wait: at least an hour,
// so a target restarted or briefly full does not fail a transfer; at most
// thirty days, so a subject is never held by a transfer for ever.
const (
	MinTransferWait = time.Hour
	MaxTransferWait = 30 * 24 * time.Hour
)

// ParseTransferWait reads transfer.wait: a duration from 1h to 30d.
func ParseTransferWait(v string) (time.Duration, error) {
	return boundedDuration(v, MinTransferWait, MaxTransferWait, "is a period from 1h to 30d, such as 24h")
}

// transferDefinitions returns the setting of transfers.
func transferDefinitions() []Definition {
	return []Definition{{
		// A day: a target restarted over a night or a weekend's quota wait
		// is waited out; one that is still refusing after a day needs a
		// person, and the subject is free again.
		Name:           TransferWait,
		Default:        "24h",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseTransferWait(v); return err },
	}}
}
