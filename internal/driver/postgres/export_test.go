package postgres

// failAttempts makes every pipeline change of t call fail after its
// function succeeded and before its commit, with the attempt's number, and
// end that attempt with the error fail returns (nil lets it commit). It is
// the only way a test sets tenantSeams.failAttempt.
func (t *Tenant) failAttempts(fail func(attempt int) error) { t.seams.failAttempt = fail }
