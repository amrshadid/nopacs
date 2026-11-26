package tenant

// ReplicaBehind is a replica's lag passing the tenant's replica.lag-alert,
// or falling back under it, the contract's message of that name: one event
// each way, never one per scan. The server reports it (AppendEvents), since
// it is a reading of the replica's rows over time, not a change of one.
type ReplicaBehind struct {
	// Replica names the replica; Primary its primary.
	Replica string `json:"replica"`
	// Primary is the name of the replica's primary.
	Primary string `json:"primary"`
	// LagMicros is the replica's lag then (how long its oldest scan not in
	// step has waited), in microseconds.
	LagMicros int64 `json:"lag_us"`
	// ScansBehind counts the scans not in step then: pending, copying or
	// damaged.
	ScansBehind int64 `json:"scans_behind"`
	// Recovered says the lag fell back under the alert.
	Recovered bool `json:"recovered"`
}

// EventType implements EventPayload.
func (*ReplicaBehind) EventType() EventType { return EventReplicaBehind }

// validate implements EventPayload.
func (p *ReplicaBehind) validate() error {
	if err := ValidateName("storage", p.Replica); err != nil {
		return err
	}
	if err := ValidateName("storage", p.Primary); err != nil {
		return err
	}
	if p.LagMicros < 0 || p.ScansBehind < 0 {
		return invalid("a replica's lag and scans behind are not negative")
	}
	return nil
}
