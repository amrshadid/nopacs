package record

// Alone returns the state a correction is taken in from its record alone,
// when the tenant's database holds no row that knows how it went: a
// rebuild finding a record with no row, a restore adopting a record its
// backup has no row of, or one whose row the backup holds still prepared
// (taken before the request was answered). It also reports whether the
// correction is to be listed unresolved, for an admin to decide.
//
// The record is read only once its MAC has verified (DecodeCorrection), so
// what it says is the tenant's own word, and it is read as it says:
//
//   - a delete committed, purging or purged hides what it names, as
//     committed and unresolved: its data is held for an admin's undo or
//     purge, never purged on a record's word, since a record may lag a
//     later step of its row (an undo whose record was not yet rewritten);
//   - a delete abandoned or undone shows its data: hiding it would withhold
//     data no one withdrew;
//   - a delete still prepared, whose outcome no one knows, is taken as
//     committed and unresolved: showing withdrawn data is the worse error;
//   - a relabel or a replace published (committed, purging or purged) is
//     committed and unresolved, its purge left to an admin;
//   - a relabel or a replace prepared or abandoned is abandoned: no row is
//     left to finish or undo its writes, and the archive is read as it was
//     before it;
//   - a move published is committed and unresolved, as a relabel; one
//     undone stays undone (its old tree is the archive again), one prepared
//     or abandoned is abandoned.
func (c Correction) Alone() (State, bool) {
	switch {
	case (c.Kind == KindDelete || c.Kind == KindMove) && (c.State == StateAbandoned || c.State == StateUndone):
		return c.State, false
	case c.Kind == KindDelete, c.State.Published():
		return StateCommitted, true
	}
	return StateAbandoned, false
}
