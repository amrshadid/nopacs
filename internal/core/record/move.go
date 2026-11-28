package record

import "github.com/amrshadid/nopacs/internal/core/label"

// KindMove files a session, with its scans, under another subject that
// exists, or merges a whole subject into another (every session moved, the
// emptied subject hidden). Like a relabel it copies every object to its key
// under the new labels before its publish and keeps the old ones until its
// purge; unlike a relabel it is undone after its publish, until its purge.
const KindMove Kind = "move"

// CopiesObjects reports whether a correction of kind k copies every object
// of its scans to keys under new labels before its publish, its old tree
// kept until its purge: a relabel and a move. Their copies are swept from
// every copy when they are not published, or taken back.
func (k Kind) CopiesObjects() bool { return k == KindRelabel || k == KindMove }

// Move says where a move files its sessions.
type Move struct {
	// Level is LevelSession for one session moved, LevelSubject for a whole
	// subject merged into another.
	Level string `json:"level"`
	// From and To are the subject's label before and the label of the
	// subject the sessions are filed under after.
	From string `json:"from"`
	To   string `json:"to"`
}

// validMove checks a move: its scans those of one session (or one subject
// for a merge), From their subject, To another subject's label, and nothing
// of another kind.
func (c Correction) validMove() error {
	m := c.Move
	if m == nil || c.Relabel != nil || c.Replace != nil || len(c.Edits) > 0 || len(c.Instances) > 0 || c.Level != "" {
		return bad("a move has its move and nothing of a relabel, a replace, an edit or a delete")
	}
	if m.Level != LevelSubject && m.Level != LevelSession {
		return bad("a move's level is subject or session")
	}
	// Another subject: a label that differs only in case is the same one.
	if !label.Valid(m.From) || !label.Valid(m.To) || label.Fold(m.From) == label.Fold(m.To) {
		return bad("a move files its sessions under another subject")
	}
	if err := c.within(m.Level); err != nil {
		return err
	}
	if c.Scans[0].Subject != m.From {
		return bad("a move's from is its scans' subject")
	}
	return nil
}
