package correct

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The attributes of a subject's identity a move rewrites when asked: the
// Patient ID and its Issuer of Patient ID (PS3.3 C.7.1.1), the pair the
// archive's subject identity is made of. The issuer's qualifiers are a
// sequence, which an edit never writes.
const (
	tagPatientID = 0x00100020
	tagIssuer    = 0x00100021
)

// subjectIdentity is a subject's identity as its archived files hold it.
type subjectIdentity struct {
	id, issuer      string
	hasID, hasIssue bool
}

// errFirstLine stops a read of a scan's metadata at its first line.
var errFirstLine = errors.New("first line read")

// identityOf reads the identity of the first instance of the scan at p on
// the storage named, from the scan's metadata: the values its line keeps
// of the archived, de-identified file.
func (e *Engine) identityOf(ctx context.Context, storage string, p label.Path) (subjectIdentity, error) {
	var out subjectIdentity
	st, err := e.cfg.OpenStorage(ctx, storage)
	if err != nil {
		return out, err
	}
	var line sidecar.Instance
	err = readLines(ctx, st, p, func(in sidecar.Instance) error {
		line = in
		return errFirstLine
	})
	if err != nil && !errors.Is(err, errFirstLine) {
		return out, err
	}
	raw, err := json.Marshal(line)
	if err != nil {
		return out, fault.Wrap(fault.Internal, err, "a line of a scan's metadata cannot be read")
	}
	values, ok := datasetValues(raw)
	if !ok {
		return out, fault.New(fault.Precondition, "a scan's metadata keeps no values of its files, so the subject's identity cannot be read")
	}
	out.id, out.hasID = values[tagKey(tagPatientID)]
	out.issuer, out.hasIssue = values[tagKey(tagIssuer)]
	return out, nil
}

// RewriteIdentity makes the identity rewrite a published move asked for
// (MoveRequest.RewriteIdentity): each session it moved whose first scan's
// archived identity is not the destination's is edited (Edit), one edit
// per session, its Patient ID set to the destination's archived one and
// its Issuer of Patient ID set to the destination's, or removed when the
// destination has none. The decision is made per session, from what its
// own metadata holds now, so a stop after some sessions' edits leaves the
// rest owed, never forgotten. Each new revision is judged by the profile
// and its check after, as every edit's is. The destination's identity is
// read from one of its own scans.
//
// The rewrite is recorded made (Moves.IdentityRewritten) only when no
// session is owed: every one holds the destination's identity, or its edit
// was refused for a reason that will not pass (the check after refused the
// new revision, or its character set would not be kept, or the session
// holds more instances than one edit changes), which is logged
// with the edit's id and, once nothing else is owed, recorded on the move
// with a fixed reason (Moves.IdentityRefused), which the move's answer
// shows. A session whose edit failed for anything else (a storage or
// database that does not answer, a prepared edit abandoned, a hold on the
// destination or the session) stays owed: the rewrite stays
// asked, and the pass tries it again (Engines' pass, through
// RewriteOwed), as does Recover at a start. A move that asked none, or is
// no longer committed, answers nothing. It answers the edits made, and
// the errors of the sessions still owed.
func (e *Engine) RewriteIdentity(ctx context.Context, id string) ([]tenant.Correction, error) {
	c, err := e.cfg.Store.Correction(ctx, id)
	if err != nil {
		return nil, err
	}
	m, err := e.moves()
	if err != nil {
		return nil, err
	}
	if c.Kind != record.KindMove || !c.RewriteIdentity {
		return nil, nil
	}
	if c.State == record.StatePrepared {
		return nil, fault.New(fault.Precondition, "the move is not yet published; its identity is rewritten once it is")
	}
	if c.State != record.StateCommitted {
		return nil, m.IdentityRewritten(ctx, id)
	}
	scans, err := e.cfg.Store.CorrectionScans(ctx, id)
	if err != nil {
		return nil, err
	}
	dest, err := e.destinationIdentity(ctx, c.ToLabel, scans)
	if err != nil {
		return nil, err
	}
	if !dest.hasID {
		// Nothing to set the moved files' Patient ID to: an edit removing
		// theirs would leave them naming no subject.
		e.cfg.Log.Warn("a move's identity rewrite is refused: the destination's files hold no Patient ID", "correction", id)
		return nil, m.IdentityRefused(ctx, id, tenant.IdentityRefusedNoID)
	}
	changes := []EditChange{{Tag: tagPatientID, Value: dest.id}}
	if dest.hasIssue {
		changes = append(changes, EditChange{Tag: tagIssuer, Value: dest.issuer})
	} else {
		changes = append(changes, EditChange{Tag: tagIssuer, Remove: true})
	}
	// The first moved scan of each session, in order: the scan whose
	// metadata says what the session holds.
	var sessions []string
	first := map[string]tenant.CorrectionScan{}
	for _, s := range scans {
		if _, ok := first[s.NewPath.Session]; !ok {
			sessions = append(sessions, s.NewPath.Session)
			first[s.NewPath.Session] = s
		}
	}
	var made []tenant.Correction
	var errs []error
	refused := ""
	for _, session := range sessions {
		s := first[session]
		moved, err := e.identityOf(ctx, s.Storage, s.NewPath)
		if err != nil {
			e.cfg.Log.Warn("a move's identity rewrite of a session is owed: its identity cannot be read", "correction", id,
				"error", fault.Message(err))
			errs = append(errs, err)
			continue
		}
		if moved == dest {
			continue
		}
		// A session whose latest identity edit was refused for good is
		// settled: it keeps its identity and is not edited again at every
		// pass while another session stays owed.
		if why, err := e.identityRefusalOf(ctx, c, session); err != nil {
			errs = append(errs, err)
			continue
		} else if why != "" {
			refused = why
			continue
		}
		if sessionTooLarge(scans, session) {
			// No edit changes so many instances: the session keeps its identity.
			e.cfg.Log.Warn("a move's identity rewrite of a session is refused: it holds more instances than one edit changes",
				"correction", id)
			refused = tenant.IdentityRefusedTooLarge
			continue
		}
		target := tenant.CorrectionTarget{Subject: c.ToLabel, Session: session}
		ed, err := e.editIdentity(ctx, c, target, changes)
		if ed.ID != "" && ed.State == record.StateCommitted {
			made = append(made, ed)
		}
		switch {
		case err == nil:
		case refusedForGood(ed):
			e.cfg.Log.Warn("a move's identity rewrite of a session was refused and is not made", "correction", id, "edit", ed.ID,
				"reason", ed.StateReason)
			refused = identityRefusal(ed)
		default:
			e.cfg.Log.Warn("a move's identity rewrite of a session is owed: its edit was not made", "correction", id, "edit", ed.ID,
				"held", errors.Is(err, tenant.ErrHeld), "error", fault.Message(err))
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		// Nothing is owed: made, or refused for good for some session, which
		// the correction says with its reason.
		var err error
		if refused != "" {
			err = m.IdentityRefused(ctx, id, refused)
		} else {
			err = m.IdentityRewritten(ctx, id)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return made, errors.Join(errs...)
}

// refusedForGood reports whether an edit made for an identity rewrite was
// abandoned for a reason that will not pass on another try: the check
// after refused its new revision, or its file's character set would not be
// kept. Anything else (storage, a hold, a prepared edit not abandoned) may.
func refusedForGood(ed tenant.Correction) bool {
	return ed.State == record.StateAbandoned && (ed.StateReason == reasonRefused || ed.StateReason == reasonCharset)
}

// sessionTooLarge reports whether the scans the move moved into session hold
// more instances than one edit changes (tenant.MaxEditInstances); a scan on
// several storages counts once.
func sessionTooLarge(scans []tenant.CorrectionScan, session string) bool {
	var n int64
	seen := map[string]bool{}
	for _, s := range scans {
		if s.NewPath.Session == session && !seen[s.SeriesUID] {
			seen[s.SeriesUID] = true
			n += s.Instances
		}
	}
	return n > tenant.MaxEditInstances
}

// identityRefusal returns the fixed phrase (tenant.IdentityRefusedProfile
// and the others) a lasting refusal of ed is recorded with.
func identityRefusal(ed tenant.Correction) string {
	if ed.StateReason == reasonCharset {
		return tenant.IdentityRefusedCharset
	}
	return tenant.IdentityRefusedProfile
}

// RewriteOwed tries again every move whose identity rewrite is asked and
// not yet made (RewriteIdentity), as the pass does each interval after
// Recover has taken up a stop's leavings: a hold released, a storage that
// answers again. It goes on past one move's failure and answers them all.
func (e *Engine) RewriteOwed(ctx context.Context) error {
	m, err := e.moves()
	if err != nil {
		return err
	}
	var errs []error
	after := ""
	for {
		page, err := m.OwedIdentityRewrites(ctx, after, pendingPage)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, c := range page {
			after = c.ID
			if _, err := e.RewriteIdentity(ctx, c.ID); err != nil {
				errs = append(errs, err)
			}
		}
		if len(page) < pendingPage {
			return errors.Join(errs...)
		}
	}
}

// editIdentity previews and makes the edit of one session of a move's
// identity rewrite, in the name of the key that asked for the move, with
// the server's audit entry.
func (e *Engine) editIdentity(ctx context.Context, c tenant.Correction, target tenant.CorrectionTarget,
	changes []EditChange) (tenant.Correction, error) {
	p, err := e.PreviewEdit(ctx, EditPreviewRequest{Target: target, Changes: changes})
	if err != nil {
		return tenant.Correction{}, err
	}
	audit := e.serverAudit("Edit", c.ID, e.now())
	audit.Object = ""
	return e.Edit(ctx, EditRequest{Target: target, Changes: changes, Confirm: p.Confirm, KeyID: c.KeyID,
		Reason: identityReason(c), Audit: *audit,
		fromArchive: true})
}

// identityReason is the reason of the edits that rewrite a move's
// identity: it names the move, so the edits of a move's session are found
// again by it.
func identityReason(c tenant.Correction) string {
	return "the subject identity of " + c.ToLabel + ", under which move " + c.ID + " filed this session"
}

// identityRefusalOf answers, for a session of the move c, the fixed phrase
// of a lasting refusal when the latest edit made for its identity rewrite
// (the edit of the destination's session with identityReason as its
// reason) was abandoned for a reason that will not pass; "" when there is
// none, or a later edit was not so abandoned.
func (e *Engine) identityRefusalOf(ctx context.Context, c tenant.Correction, session string) (string, error) {
	var latest tenant.Correction
	page := master.Page{Limit: 100}
	for {
		got, next, err := e.cfg.Store.ListCorrections(ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindEdit}}, page)
		if err != nil {
			return "", err
		}
		for _, ed := range got {
			if ed.Reason == identityReason(c) && ed.Target.Subject == c.ToLabel && ed.Target.Session == session && ed.ID > latest.ID {
				latest = ed
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if !refusedForGood(latest) {
		return "", nil
	}
	return identityRefusal(latest), nil
}

// destinationIdentity reads the destination's archived identity from one
// of its scans the move did not move.
func (e *Engine) destinationIdentity(ctx context.Context, to string, moved []tenant.CorrectionScan) (subjectIdentity, error) {
	reads, ok := e.cfg.Store.(tenant.ScanReads)
	if !ok {
		return subjectIdentity{}, fault.New(fault.Unavailable, "the tenant's database answers no reads of its scans")
	}
	series := make(map[string]bool, len(moved))
	for _, s := range moved {
		series[s.SeriesUID] = true
	}
	page := master.Page{Limit: 100}
	for {
		got, err := reads.ListScans(ctx, tenant.ScanFilter{SubjectLabel: to, Order: tenant.OrderLabel}, page)
		if err != nil {
			return subjectIdentity{}, err
		}
		for _, s := range got.Items {
			if !series[s.SeriesUID] {
				return e.identityOf(ctx, s.Storage, label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
			}
		}
		if got.Next == "" {
			return subjectIdentity{}, fault.New(fault.Precondition, "the destination holds no scan of its own to read its identity from")
		}
		page.Cursor = got.Next
	}
}
