package sqlkit

import (
	"context"
	"time"
)

// LowerCreated keeps a commit's scan, session and subject created at the
// earliest held time of the instances beneath them, after the commit added
// instances held at at: a row's created time only moves down, to at when
// it is earlier. Commits of one session may land out of the order of their
// held times (each takes its held time from the caller's clock before its
// transaction), so the first row made is not always the earliest; a
// rebuild derives each row's created time from the rows beneath it (a
// scan's from its first instance, a session's from its earliest scan, a
// subject's from its earliest session), and the catalog in use must be the
// one a rebuild makes. A session's modalities carry its created time, so
// they move with it. A driver calls it in the commit's transaction after
// the commit's rows were locked (scan, session, the session's modalities,
// subject), and in that order, so it takes no lock out of the order every
// commit keeps.
func (k Sending) LowerCreated(ctx context.Context, tx SendingTx, at time.Time, scanID, sessionID, subjectID int64) error {
	t := k.D.Time(at)
	for _, q := range []struct {
		sql string
		id  int64
	}{
		{`UPDATE scans SET created = ? WHERE id = ? AND created > ?`, scanID},
		{`UPDATE sessions SET created = ? WHERE id = ? AND created > ?`, sessionID},
		{`UPDATE session_modalities SET created = ? WHERE session_id = ? AND created > ?`, sessionID},
		{`UPDATE subjects SET created = ? WHERE id = ? AND created > ?`, subjectID},
	} {
		if _, err := tx.Exec(ctx, q.sql, t, q.id, t); err != nil {
			return err
		}
	}
	return nil
}
