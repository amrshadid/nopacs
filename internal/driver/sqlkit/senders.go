package sqlkit

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ListSenders is the senders' list's name, the first field of its cursors.
const ListSenders = "tenant-senders"

// senderColumns are a sender's columns as a read returns them: the
// credential only as whether one is held, so no read can carry it out.
const senderColumns = `name, type, enabled, config, credential IS NOT NULL, max_associations, max_bytes_per_second,
	connect_us, response_us, created, updated`

// senderConfig is a sender's configuration of its type as its config column
// holds it, written and read here so both drivers keep the same text. The
// field names are the column's contract and never change; a field added
// later is read as its zero value from an older row.
type senderConfig struct {
	Host                   string           `json:"host,omitempty"`
	Port                   int              `json:"port,omitempty"`
	CalledAETitle          string           `json:"called_ae_title,omitempty"`
	CallingAETitle         string           `json:"calling_ae_title,omitempty"`
	URL                    string           `json:"url,omitempty"`
	Authentication         string           `json:"authentication,omitempty"`
	Username               string           `json:"username,omitempty"`
	HeaderName             string           `json:"header_name,omitempty"`
	MaxInstancesPerRequest int              `json:"max_instances_per_request,omitempty"`
	MaxRequestBytes        int64            `json:"max_request_bytes,omitempty"`
	TLS                    *senderTLSConfig `json:"tls,omitempty"`
}

// senderTLSConfig is a sender's TLS files and server name.
type senderTLSConfig struct {
	CAFile          string `json:"ca_file"`
	CertificateFile string `json:"certificate_file"`
	PrivateKeyFile  string `json:"private_key_file"`
	ServerName      string `json:"server_name"`
}

// senderConfigText returns the text of a sender's config column.
func senderConfigText(s tenant.Sender) (string, error) {
	c := senderConfig{Host: s.DIMSE.Host, Port: s.DIMSE.Port, CalledAETitle: s.DIMSE.CalledAETitle,
		CallingAETitle: s.DIMSE.CallingAETitle, URL: s.STOW.URL, Authentication: string(s.STOW.Authentication),
		Username: s.STOW.Username, HeaderName: s.STOW.HeaderName, MaxInstancesPerRequest: s.STOW.MaxInstancesPerRequest,
		MaxRequestBytes: s.STOW.MaxRequestBytes}
	t := s.DIMSE.TLS
	if s.Type == tenant.SenderSTOW {
		t = s.STOW.TLS
	}
	if t != nil {
		tc := senderTLSConfig(*t)
		c.TLS = &tc
	}
	return JSONText(c)
}

// scanSender reads one row of senderColumns.
func scanSender(row Row) (tenant.Sender, error) {
	var s tenant.Sender
	var typ, config string
	var set bool
	var connect, response int64
	var created, updated DBTime
	if err := row.Scan(&s.Name, &typ, &s.Enabled, &config, &set, &s.MaxAssociations, &s.MaxBytesPerSecond, &connect,
		&response, &created, &updated); err != nil {
		return s, err
	}
	s.Type = tenant.SenderType(typ)
	s.ConnectTimeout, s.ResponseTimeout = time.Duration(connect)*time.Microsecond, time.Duration(response)*time.Microsecond
	s.Created, s.Updated = created.T, updated.T
	var c senderConfig
	if err := FromJSON(config, &c); err != nil {
		return s, err
	}
	var t *tenant.SenderTLS
	if c.TLS != nil {
		v := tenant.SenderTLS(*c.TLS)
		t = &v
	}
	switch s.Type {
	case tenant.SenderDIMSE:
		s.DIMSE = tenant.DIMSESender{Host: c.Host, Port: c.Port, CalledAETitle: c.CalledAETitle, CallingAETitle: c.CallingAETitle, TLS: t}
	case tenant.SenderSTOW:
		s.STOW = tenant.STOWSender{URL: c.URL, Authentication: tenant.StowAuthentication(c.Authentication), Username: c.Username,
			HeaderName: c.HeaderName, CredentialSet: set, TLS: t, MaxInstancesPerRequest: c.MaxInstancesPerRequest,
			MaxRequestBytes: c.MaxRequestBytes}
	}
	return s, nil
}

// senderInTx reads a sender inside a change's transaction with the lock
// given, or NotFound.
func senderInTx(ctx context.Context, tx SendingTx, name, lock string) (tenant.Sender, error) {
	var s tenant.Sender
	var scanErr error
	found := false
	err := tx.Query(ctx, `SELECT `+senderColumns+` FROM senders WHERE name = ?`+lock, []any{name}, func(r Row) error {
		found = true
		s, scanErr = scanSender(r)
		return scanErr
	})
	if err != nil {
		return s, err
	}
	if !found {
		return s, notFound("the tenant has no sender named " + name)
	}
	return s, nil
}

// exists reports whether query returns a row.
func exists(ctx context.Context, tx SendingTx, query string, args ...any) (bool, error) {
	var one int
	return tx.QueryRow(ctx, query, args, &one)
}

// senderArgs are a sender's values for its columns but its name, type and
// credential, in the order of an UPDATE's SET list.
func (d SendingDialect) senderArgs(s tenant.Sender) ([]any, error) {
	config, err := senderConfigText(s)
	if err != nil {
		return nil, err
	}
	return []any{d.Bool(s.Enabled), config, s.MaxAssociations, s.MaxBytesPerSecond, s.ConnectTimeout.Microseconds(),
		s.ResponseTimeout.Microseconds(), d.Time(s.Updated)}, nil
}

// AddSender implements tenant.Senders.
func (k Sending) AddSender(ctx context.Context, s tenant.Sender, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Sender, error) {
	s = s.Normalize()
	s.STOW.CredentialSet = false
	if err := s.Validate(); err != nil {
		return tenant.Sender{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Sender{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Sender{}, err
	}
	s.Created, s.Updated = now, now
	args, err := k.D.senderArgs(s)
	if err != nil {
		return tenant.Sender{}, err
	}
	var credential any
	if len(s.STOW.SealedCredential) > 0 {
		credential = s.STOW.SealedCredential
	}
	// Where it sends and its type, never its credential (Sender.Destination).
	a, err := AddDetails(audit, tenant.DetailSenderType, string(s.Type), tenant.DetailSenderDestination, s.Destination())
	if err != nil {
		return tenant.Sender{}, err
	}
	var out tenant.Sender
	err = k.Host.Write(ctx, "the sender cannot be added", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		taken, err := exists(ctx, tx, `SELECT 1 FROM senders WHERE name = ?`, s.Name)
		if err != nil {
			return err
		}
		if taken {
			return fault.New(fault.Conflict, "the tenant has a sender named "+s.Name)
		}
		held, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE sender = ? LIMIT 1`, s.Name)
		if err != nil {
			return err
		}
		if held {
			return fault.New(fault.Conflict, "the name "+s.Name+" is still named by jobs of a sender removed")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO senders (name, type, enabled, config, max_associations, max_bytes_per_second,
			connect_us, response_us, updated, created, credential) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			append(append([]any{s.Name, string(s.Type)}, args...), k.D.Time(now), credential)...); err != nil {
			return err
		}
		if out, err = senderInTx(ctx, tx, s.Name, ""); err != nil {
			return err
		}
		return tx.Audited(ctx, a, guard)
	})
	if err != nil {
		return tenant.Sender{}, err
	}
	return out, nil
}

// Sender implements tenant.Senders.
func (k Sending) Sender(ctx context.Context, name string) (tenant.Sender, error) {
	if err := tenant.ValidateSenderName(name); err != nil {
		return tenant.Sender{}, err
	}
	var out tenant.Sender
	err := k.Host.Read(ctx, "the sender cannot be read", func(tx SendingTx) error {
		var err error
		out, err = senderInTx(ctx, tx, name, "")
		return err
	})
	return out, err
}

// SenderCredential implements tenant.Senders. The sealed bytes are read as
// stored and handed back as they are; nothing here opens or logs them.
func (k Sending) SenderCredential(ctx context.Context, name string) ([]byte, error) {
	if err := tenant.ValidateSenderName(name); err != nil {
		return nil, err
	}
	var out []byte
	err := k.Host.Read(ctx, "the sender's credential cannot be read", func(tx SendingTx) error {
		found, err := tx.QueryRow(ctx, `SELECT credential FROM senders WHERE name = ? AND credential IS NOT NULL`, []any{name}, &out)
		if err == nil && !found {
			return notFound("the tenant has no sender named " + name + " that holds a credential")
		}
		return err
	})
	return out, err
}

// ListSenders implements tenant.Senders.
func (k Sending) ListSenders(ctx context.Context, page master.Page) ([]tenant.Sender, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	after := ""
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListSenders, k.Host.Binding(), 1)
		if err != nil {
			return nil, "", err
		}
		after = f[0]
	}
	var out []tenant.Sender
	err := k.Host.Read(ctx, "the senders cannot be listed", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT `+senderColumns+` FROM senders WHERE name > ? ORDER BY name LIMIT ?`,
			[]any{after, page.Limit + 1}, func(r Row) error {
				s, err := scanSender(r)
				out = append(out, s)
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
	return out, EncodeCursor(ListSenders, k.Host.Binding(), last.Name), nil
}

// SetSender implements tenant.Senders.
func (k Sending) SetSender(ctx context.Context, s tenant.Sender, fields []tenant.SenderField, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Sender, error) {
	if err := tenant.ValidateSenderName(s.Name); err != nil {
		return tenant.Sender{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Sender{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Sender{}, err
	}
	var out tenant.Sender
	err := k.Host.Write(ctx, "the sender cannot be changed", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		stored, err := senderInTx(ctx, tx, s.Name, k.D.Lock)
		if err != nil {
			return err
		}
		next, _, cleared, err := tenant.ApplySender(stored, s, fields)
		if err != nil {
			return err
		}
		next.Updated = now
		args, err := k.D.senderArgs(next)
		if err != nil {
			return err
		}
		var credential any
		if len(next.STOW.SealedCredential) > 0 {
			credential = next.STOW.SealedCredential
		}
		// The credential is written only when the change names it
		// (coalesce keeps the held one otherwise), and removed when the
		// authentication became none.
		if _, err := tx.Exec(ctx, `UPDATE senders SET enabled = ?, config = ?, max_associations = ?, max_bytes_per_second = ?,
			connect_us = ?, response_us = ?, updated = ?, credential = coalesce(?, credential) WHERE name = ?`,
			append(args, credential, s.Name)...); err != nil {
			return err
		}
		if cleared {
			if _, err := tx.Exec(ctx, `UPDATE senders SET credential = NULL WHERE name = ?`, s.Name); err != nil {
				return err
			}
		}
		if out, err = senderInTx(ctx, tx, s.Name, ""); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	if err != nil {
		return tenant.Sender{}, err
	}
	return out, nil
}

// RemoveSender implements tenant.Senders.
func (k Sending) RemoveSender(ctx context.Context, name string, cancelJobs bool, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Sender, int64, error) {
	if err := tenant.ValidateSenderName(name); err != nil {
		return tenant.Sender{}, 0, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Sender{}, 0, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Sender{}, 0, err
	}
	var out tenant.Sender
	var canceled int64
	err := k.Host.Write(ctx, "the sender cannot be removed", func(tx SendingTx) error {
		canceled = 0
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		// The row is locked for its removal first, so a forward that
		// would make a job for it waits and then finds it gone.
		stored, err := senderInTx(ctx, tx, name, k.D.LockDelete)
		if err != nil {
			return err
		}
		if receiver, err := receiverForwardingTo(ctx, tx, name); err != nil || receiver != "" {
			if err != nil {
				return err
			}
			return tenant.Precondition("the receiver " + receiver + " forwards to this sender")
		}
		var ids []string
		if err := tx.Query(ctx, k.D.SenderJobsInProgressSQL(), []any{name}, func(r Row) error {
			var id string
			err := r.Scan(&id)
			ids = append(ids, id)
			return err
		}); err != nil {
			return err
		}
		if len(ids) > 0 && !cancelJobs {
			return tenant.Precondition("the sender has " + strconv.Itoa(len(ids)) + " jobs queued or running; cancel them first, or remove it canceling them")
		}
		var events []tenant.EventRecord
		for _, id := range ids {
			_, e, err := k.cancelInTx(ctx, tx, id, tenant.JobReasonSenderRemoved, now)
			if err != nil {
				return err
			}
			if e != nil {
				events = append(events, *e)
				canceled++
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM senders WHERE name = ?`, name); err != nil {
			return err
		}
		out = stored
		a, err := AddDetails(audit, tenant.DetailSenderType, string(stored.Type), tenant.DetailJobsCanceled,
			strconv.FormatInt(canceled, 10))
		if err != nil {
			return err
		}
		if err := tx.WriteEvents(ctx, events...); err != nil {
			return err
		}
		return tx.Audited(ctx, a, guard)
	})
	if err != nil {
		return tenant.Sender{}, 0, err
	}
	return out, canceled, nil
}

// receiverForwardingTo returns the name of a receiver that forwards to the
// sender, itself or by one of its routes, or "".
func receiverForwardingTo(ctx context.Context, tx SendingTx, sender string) (string, error) {
	return receiverWhose(ctx, tx, func(c dimseConfig) bool {
		return slices.Contains(c.ForwardTo, sender) || slices.Contains(routingOf(c).Senders(), sender)
	})
}

// CheckForwardTo refuses a receiver's ForwardTo that names a sender the
// tenant does not have, as a precondition; each sender's row is held from
// removal to the commit (SendingDialect.Share). A driver calls it in the
// transaction that adds or changes the receiver.
func CheckForwardTo(ctx context.Context, tx SendingTx, d SendingDialect, senders []string) error {
	for _, s := range senders {
		has, err := exists(ctx, tx, `SELECT 1 FROM senders WHERE name = ?`+d.Share, s)
		if err != nil {
			return err
		}
		if !has {
			return tenant.Precondition("the tenant has no sender named " + s)
		}
	}
	return nil
}
