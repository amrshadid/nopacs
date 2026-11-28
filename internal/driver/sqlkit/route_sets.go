package sqlkit

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ListRuleSets is the list a cursor of a receiver's rule set versions is
// bound to.
const ListRuleSets = "tenant-rule-sets"

// A rule set version is a row of route_rule_sets, its body the JSON
// RuleSetText writes, never updated: the table's trigger refuses an
// UPDATE, so a reception's record of the version it was routed by always
// means the rules it was routed by. The receiver's config column copies
// the body of the version it runs (ruleSetConfig), so the pipeline reads a
// receiver's rules with the receiver.

// errNoReceiver answers a receiver the tenant does not have.
func errNoReceiver(name string) error {
	return fault.New(fault.NotFound, "the tenant has no receiver named "+name)
}

// errNoRuleSet answers a version a receiver does not have.
func errNoRuleSet(receiver string, version int32) error {
	return fault.New(fault.NotFound, "the receiver "+receiver+" has no rule set version "+strconv.Itoa(int(version)))
}

// ruleSetColumns are what a read of route_rule_sets scans (scanRuleSet).
const ruleSetColumns = `receiver, version, body, created, created_by`

// scanRuleSet reads one row of ruleSetColumns.
func scanRuleSet(r Row) (tenant.RuleSet, error) {
	var receiver, body, by string
	var version int64
	var created DBTime
	if err := r.Scan(&receiver, &version, &body, &created, &by); err != nil {
		return tenant.RuleSet{}, err
	}
	rs, err := ReadRuleSet(receiver, body)
	if err != nil {
		return rs, err
	}
	rs.Version = int32(version) // #nosec G115 -- the table's check bounds it.
	rs.Created, rs.CreatedBy = created.T, by
	return rs, nil
}

// receiverConfigIn reads the receiver's name, profile and rule set from
// its config column in tx, locked for a change, or NotFound.
func (k Sending) receiverConfigIn(ctx context.Context, tx SendingTx, name string) (tenant.Receiver, error) {
	var config, naming string
	found, err := tx.QueryRow(ctx, `SELECT config, naming FROM receivers WHERE name = ?`+k.D.Lock, []any{name}, &config, &naming)
	if err != nil {
		return tenant.Receiver{}, err
	}
	if !found {
		return tenant.Receiver{}, errNoReceiver(name)
	}
	r := tenant.Receiver{Name: name}
	return r, ReadReceiverConfig(&r, config, naming)
}

// runRuleSet points the receiver at rs (the zero rule set for none) in tx,
// under the rules a receiver's change applies to what it names: an archive
// rule's storage is the tenant's and an archive target, a forward rule's
// sender the tenant's, and an archive rule's profile only on a receiver
// that de-identifies (tenant.Receiver.Validate).
func (k Sending) runRuleSet(ctx context.Context, tx SendingTx, r tenant.Receiver, rs tenant.RuleSet, now time.Time) error {
	if err := r.CheckRuleSet(rs); err != nil {
		return err
	}
	at := "rule set version " + strconv.Itoa(int(rs.Version))
	for _, s := range rs.Storages() {
		if err := CheckArchiveTarget(ctx, tx, at, s); err != nil {
			return err
		}
	}
	if err := CheckForwardTo(ctx, tx, k.D, rs.Senders()); err != nil {
		return fault.Wrap(fault.KindOf(err), err, at+": "+fault.Message(err))
	}
	var config string
	found, err := tx.QueryRow(ctx, `SELECT config FROM receivers WHERE name = ?`, []any{r.Name}, &config)
	if err != nil {
		return err
	}
	if !found {
		return errNoReceiver(r.Name)
	}
	var c dimseConfig
	if err := FromJSON(config, &c); err != nil {
		return err
	}
	// Only the rule set changes: the rest of the column is written back as
	// it was read, so no field this version does not know is lost.
	c.RuleSet = ruleSetConfigOf(rs)
	text, err := JSONText(c)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE receivers SET config = ?, updated = ? WHERE name = ?`, text, k.D.Time(now), r.Name)
	return err
}

// CreateRuleSet implements tenant.Routing.
func (k Sending) CreateRuleSet(ctx context.Context, rs tenant.RuleSet, activate bool, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.RuleSet, error) {
	rs.Version = 0
	if err := rs.Validate(); err != nil {
		return tenant.RuleSet{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.RuleSet{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.RuleSet{}, err
	}
	var out tenant.RuleSet
	err := k.Host.Write(ctx, "the rule set cannot be created", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		r, err := k.receiverConfigIn(ctx, tx, rs.Receiver)
		if err != nil {
			return err
		}
		var last int64
		if _, err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM route_rule_sets WHERE receiver = ?`, []any{rs.Receiver}, &last); err != nil {
			return err
		}
		if last >= tenant.MaxRuleSetVersion {
			return tenant.Precondition("the receiver has made its last rule set version")
		}
		next := rs.Clone()
		next.Version = int32(last + 1) // #nosec G115 -- bounded just above.
		next.Created, next.CreatedBy = now, audit.KeyID
		body, err := RuleSetText(next)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO route_rule_sets (`+ruleSetColumns+`) VALUES (?, ?, ?, ?, ?)`,
			next.Receiver, next.Version, body, k.D.Time(now), audit.KeyID); err != nil {
			return err
		}
		if activate {
			if err := k.runRuleSet(ctx, tx, r, next, now); err != nil {
				return err
			}
		}
		out = next
		return tx.Audited(ctx, withObject(audit, next.Receiver+"@"+strconv.Itoa(int(next.Version))), guard)
	})
	return out, err
}

// ActivateRuleSet implements tenant.Routing.
func (k Sending) ActivateRuleSet(ctx context.Context, receiver string, version int32, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.RuleSet, error) {
	if err := tenant.ValidateName("receiver", receiver); err != nil {
		return tenant.RuleSet{}, err
	}
	if version < 0 || version > tenant.MaxRuleSetVersion {
		return tenant.RuleSet{}, fault.New(fault.Invalid, "a rule set's version is from 1 to "+strconv.Itoa(tenant.MaxRuleSetVersion)+", or 0 for none")
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.RuleSet{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.RuleSet{}, err
	}
	var out tenant.RuleSet
	err := k.Host.Write(ctx, "the rule set cannot be activated", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		r, err := k.receiverConfigIn(ctx, tx, receiver)
		if err != nil {
			return err
		}
		out = tenant.RuleSet{}
		if version != 0 {
			var found bool
			err := tx.Query(ctx, `SELECT `+ruleSetColumns+` FROM route_rule_sets WHERE receiver = ? AND version = ?`, []any{receiver, version},
				func(row Row) error {
					rs, err := scanRuleSet(row)
					out, found = rs, true
					return err
				})
			if err != nil {
				return err
			}
			if !found {
				return errNoRuleSet(receiver, version)
			}
		}
		if err := k.runRuleSet(ctx, tx, r, out, now); err != nil {
			return err
		}
		return tx.Audited(ctx, withObject(audit, receiver+"@"+strconv.Itoa(int(version))), guard)
	})
	return out, err
}

// RuleSet implements tenant.Routing.
func (k Sending) RuleSet(ctx context.Context, receiver string, version int32) (tenant.RuleSet, error) {
	if err := tenant.ValidateName("receiver", receiver); err != nil {
		return tenant.RuleSet{}, err
	}
	if version < 0 || version > tenant.MaxRuleSetVersion {
		return tenant.RuleSet{}, fault.New(fault.Invalid, "a rule set's version is from 1 to "+strconv.Itoa(tenant.MaxRuleSetVersion)+", or 0 for the one it runs")
	}
	var out tenant.RuleSet
	err := k.Host.Read(ctx, "the rule set cannot be read", func(tx SendingTx) error {
		var config string
		found, err := tx.QueryRow(ctx, `SELECT config FROM receivers WHERE name = ?`, []any{receiver}, &config)
		if err != nil {
			return err
		}
		if version == 0 {
			if !found {
				return errNoReceiver(receiver)
			}
			var c dimseConfig
			if err := FromJSON(config, &c); err != nil {
				return err
			}
			if c.RuleSet == nil {
				return fault.New(fault.NotFound, "the receiver "+receiver+" runs no rule set")
			}
			version = c.RuleSet.Version
		}
		var hit bool
		err = tx.Query(ctx, `SELECT `+ruleSetColumns+` FROM route_rule_sets WHERE receiver = ? AND version = ?`, []any{receiver, version},
			func(row Row) error {
				rs, err := scanRuleSet(row)
				out, hit = rs, true
				return err
			})
		if err != nil {
			return err
		}
		if !hit {
			if !found {
				return errNoReceiver(receiver)
			}
			return errNoRuleSet(receiver, version)
		}
		return nil
	})
	return out, err
}

// ListRuleSets implements tenant.Routing.
func (k Sending) ListRuleSets(ctx context.Context, receiver string, page master.Page) ([]tenant.RuleSet, string, error) {
	if err := tenant.ValidateName("receiver", receiver); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.Host.Binding(receiver)
	before := int64(tenant.MaxRuleSetVersion) + 1
	if page.Cursor != "" {
		fs, err := DecodeCursor(page.Cursor, ListRuleSets, binding, 1)
		if err != nil {
			return nil, "", err
		}
		if before, err = DecodeCursorInt(fs[0]); err != nil {
			return nil, "", err
		}
	}
	var out []tenant.RuleSet
	err := k.Host.Read(ctx, "the rule sets cannot be listed", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT `+ruleSetColumns+` FROM route_rule_sets WHERE receiver = ? AND version < ? ORDER BY version DESC LIMIT ?`,
			[]any{receiver, before, page.Limit + 1}, func(row Row) error {
				rs, err := scanRuleSet(row)
				out = append(out, rs)
				return err
			})
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListRuleSets, binding, strconv.Itoa(int(last.Version))), nil
}

// RouteHits implements tenant.Routing.
func (k Sending) RouteHits(ctx context.Context, receiver string, version int32) ([]tenant.RouteHit, error) {
	if err := tenant.ValidateName("receiver", receiver); err != nil {
		return nil, err
	}
	if version < 0 || version > tenant.MaxRuleSetVersion {
		return nil, fault.New(fault.Invalid, "a rule set's version is from 1 to "+strconv.Itoa(tenant.MaxRuleSetVersion)+", or 0 for every version")
	}
	q, args := `SELECT version, phase, rule, COUNT(*), COALESCE(SUM(series), 0), MAX(matched_at) FROM route_matches WHERE receiver = ?`, []any{receiver}
	if version != 0 {
		q, args = q+` AND version = ?`, append(args, version)
	}
	q += ` GROUP BY version, phase, rule ORDER BY version, phase, rule`
	var out []tenant.RouteHit
	err := k.Host.Read(ctx, "the rules' hits cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, q, args, func(row Row) error {
			var h tenant.RouteHit
			var v int64
			var phase string
			var last DBTime
			if err := row.Scan(&v, &phase, &h.Rule, &h.Receptions, &h.Series, &last); err != nil {
				return err
			}
			h.Version, h.Phase, h.Last = int32(v), tenant.RulePhase(phase), last.T // #nosec G115 -- the table's check bounds it.
			out = append(out, h)
			return nil
		})
	})
	return out, err
}

// RecentSessions implements tenant.Routing.
func (k Sending) RecentSessions(ctx context.Context, receiver string, limit int) ([]tenant.RoutedSession, error) {
	if err := tenant.ValidateName("receiver", receiver); err != nil {
		return nil, err
	}
	if limit < 1 || limit > tenant.MaxDryRunSessions {
		return nil, fault.New(fault.Invalid, "a dry run reads from 1 to "+strconv.Itoa(tenant.MaxDryRunSessions)+" sessions")
	}
	var out []tenant.RoutedSession
	err := k.Host.Read(ctx, "the receiver's sessions cannot be read", func(tx SendingTx) error {
		out = nil
		err := tx.Query(ctx, `SELECT subject_label, label, study_uid, storage, route, calling_ae, called_ae, created FROM sessions
			WHERE receiver = ? ORDER BY created DESC, id DESC LIMIT ?`, []any{receiver, limit}, func(row Row) error {
			var s tenant.RoutedSession
			var route string
			var created DBTime
			if err := row.Scan(&s.SubjectLabel, &s.SessionLabel, &s.StudyUID, &s.Storage, &route, &s.Route.CallingAE, &s.Route.CalledAE,
				&created); err != nil {
				return err
			}
			r, err := ReadReceptionRoute(route)
			if err != nil {
				return err
			}
			calling, called := s.Route.CallingAE, s.Route.CalledAE
			s.Route = r.Archived()
			s.Route.CallingAE, s.Route.CalledAE = calling, called
			s.Created = created.T
			out = append(out, s)
			return nil
		})
		if err != nil {
			return err
		}
		for i := range out {
			seen := map[string]bool{}
			err := tx.Query(ctx, `SELECT m.version, m.phase, m.rule, m.series FROM route_matches AS m
				WHERE m.reception_id IN (SELECT id FROM receptions WHERE study_uid = ? AND receiver = ?)
				ORDER BY m.version DESC, m.phase, m.rule`, []any{out[i].StudyUID, receiver}, func(row Row) error {
				var m tenant.RouteMatch
				var v int64
				var phase string
				if err := row.Scan(&v, &phase, &m.Rule, &m.Series); err != nil {
					return err
				}
				m.Version, m.Phase = int32(v), tenant.RulePhase(phase) // #nosec G115 -- the table's check bounds it.
				if key := phase + "\x00" + m.Rule; !seen[key] {
					seen[key] = true
					out[i].Matches = append(out[i].Matches, m)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// RecordRouteMatches writes, in the transaction of a reception's identify
// step, the records of the rules route says the reception was routed by
// (tenant.ReceptionRoute.Matches): what hit counts and a dry run read. A
// step run again writes nothing twice.
func RecordRouteMatches(ctx context.Context, tx SendingTx, d SendingDialect, receptionID, receiver string, route tenant.ReceptionRoute, at time.Time) error {
	for _, m := range route.Matches() {
		if _, err := tx.Exec(ctx, `INSERT INTO route_matches (reception_id, receiver, version, phase, rule, series, matched_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, receptionID, receiver, m.Version, string(m.Phase), m.Rule, m.Series,
			d.Time(at)); err != nil {
			return err
		}
	}
	return nil
}
