package sqlkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A tenant's de-identification profiles and lookup tables
// (tenant.Deidentification) on a SQL database, one implementation both
// drivers answer with, run in the same transactions as the senders and
// jobs (SendingHost, SendingTx, SendingDialect): the statements are written
// once, with "?" for each parameter.
//
// A version's texts are kept as bytes (BLOB, bytea), never as a JSON type a
// database would normalize, so they are answered byte for byte as they were
// given and their checksum holds on every read.

// The lists' names, the first field of their cursors.
const (
	ListProfiles        = "tenant-profiles"
	ListProfileVersions = "tenant-profile-versions"
)

// Deidentification is tenant.Deidentification on a SQL database: the driver
// gives its host and dialect, and calls these from its own methods.
type Deidentification struct {
	// Host runs the transactions.
	Host SendingHost
	// D writes the statements in the database's terms.
	D SendingDialect
}

// profileColumns are a version's columns but its texts.
const profileColumns = `name, version, checksum, note, created_by_key, created`

// scanProfile reads a row of profileColumns, and of the texts after them
// when texts is set.
func scanProfile(row Row, texts bool) (tenant.ProfileVersion, error) {
	var v tenant.ProfileVersion
	var created DBTime
	dest := []any{&v.Name, &v.Version, &v.Checksum, &v.Note, &v.CreatedByKeyID, &created}
	if texts {
		dest = append(dest, &v.Source, &v.Expanded)
	}
	if err := row.Scan(dest...); err != nil {
		return v, err
	}
	v.Created = created.T
	if texts {
		// The checksum is checked on every read that answers the texts, so
		// a row changed outside the store (an edit, a restore gone wrong)
		// is never answered, or run, as the version it claims to be.
		sum := sha256.Sum256(v.Expanded)
		if hex.EncodeToString(sum[:]) != v.Checksum {
			return tenant.ProfileVersion{}, fault.New(fault.Internal, "the stored profile version "+v.Ref().String()+
				" does not match its checksum")
		}
	}
	return v, nil
}

// latestInTx reads the profile's latest version with its texts, or nil.
func latestInTx(ctx context.Context, tx SendingTx, name string) (*tenant.ProfileVersion, error) {
	var out *tenant.ProfileVersion
	err := tx.Query(ctx, `SELECT `+profileColumns+`, source, expanded FROM profile_versions WHERE name = ?
		ORDER BY version DESC LIMIT 1`, []any{name}, func(r Row) error {
		v, err := scanProfile(r, true)
		out = &v
		return err
	})
	return out, err
}

// CreateProfileVersion implements tenant.Profiles. The change is serialized
// with every other change of the tenant's configuration (LockConfig), so
// the latest version read is the latest at the commit.
func (k Deidentification) CreateProfileVersion(ctx context.Context, v tenant.NewProfileVersion, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.ProfileVersion, bool, error) {
	if err := v.Validate(); err != nil {
		return tenant.ProfileVersion{}, false, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.ProfileVersion{}, false, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.ProfileVersion{}, false, err
	}
	var out tenant.ProfileVersion
	var created bool
	err := k.Host.Write(ctx, "the profile version cannot be stored", func(tx SendingTx) error {
		created = false
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		latest, err := latestInTx(ctx, tx, v.Name)
		if err != nil {
			return err
		}
		next, err := tenant.CreateOutcome(latest, v)
		if err != nil {
			return err
		}
		if next == 0 {
			out = *latest
		} else {
			out = tenant.ProfileVersion{Name: v.Name, Version: next, Checksum: v.Checksum, Note: v.Note,
				CreatedByKeyID: audit.KeyID, Created: time.UnixMicro(now.UnixMicro()).UTC(), Source: slices.Clone(v.Source), Expanded: slices.Clone(v.Expanded)}
			if _, err := tx.Exec(ctx, `INSERT INTO profile_versions (name, version, checksum, note, created_by_key, created,
				source, expanded) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, v.Name, next, v.Checksum, v.Note, audit.KeyID,
				k.D.Time(now), v.Source, v.Expanded); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO profiles (name, latest_version, latest_created) VALUES (?, ?, ?)
				ON CONFLICT (name) DO UPDATE SET latest_version = excluded.latest_version, latest_created = excluded.latest_created`,
				v.Name, next, k.D.Time(now)); err != nil {
				return err
			}
			created = true
		}
		a, err := tenant.CreatedDetails(audit, out, created)
		if err != nil {
			return err
		}
		return tx.Audited(ctx, a, guard)
	})
	if err != nil {
		return tenant.ProfileVersion{}, false, err
	}
	return out, created, nil
}

// ProfileVersion implements tenant.Profiles.
func (k Deidentification) ProfileVersion(ctx context.Context, name string, version int32) (tenant.ProfileVersion, error) {
	if err := tenant.ValidateName("profile", name); err != nil {
		return tenant.ProfileVersion{}, err
	}
	if version < 0 {
		return tenant.ProfileVersion{}, fault.New(fault.Invalid, "a profile's version is 0, for the latest, or more")
	}
	var out *tenant.ProfileVersion
	err := k.Host.Read(ctx, "the profile version cannot be read", func(tx SendingTx) error {
		out = nil
		if version == 0 {
			var err error
			out, err = latestInTx(ctx, tx, name)
			return err
		}
		return tx.Query(ctx, `SELECT `+profileColumns+`, source, expanded FROM profile_versions WHERE name = ? AND version = ?`,
			[]any{name, version}, func(r Row) error {
				v, err := scanProfile(r, true)
				out = &v
				return err
			})
	})
	if err != nil {
		return tenant.ProfileVersion{}, err
	}
	if out == nil {
		return tenant.ProfileVersion{}, notFound("the tenant has no such version of the profile " + name)
	}
	return *out, nil
}

// ListProfiles implements tenant.Profiles.
func (k Deidentification) ListProfiles(ctx context.Context, page master.Page) ([]tenant.ProfileSummary, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	after := ""
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListProfiles, k.Host.Binding(), 1)
		if err != nil {
			return nil, "", err
		}
		after = f[0]
	}
	var out []tenant.ProfileSummary
	err := k.Host.Read(ctx, "the profiles cannot be listed", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT name, latest_version, latest_created FROM profiles WHERE name > ? ORDER BY name LIMIT ?`,
			[]any{after, page.Limit + 1}, func(r Row) error {
				var s tenant.ProfileSummary
				var created DBTime
				err := r.Scan(&s.Name, &s.LatestVersion, &created)
				s.LatestCreated = created.T
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
	return out, EncodeCursor(ListProfiles, k.Host.Binding(), last.Name), nil
}

// ListProfileVersions implements tenant.Profiles.
func (k Deidentification) ListProfileVersions(ctx context.Context, name string, page master.Page) ([]tenant.ProfileVersion, string, error) {
	if err := tenant.ValidateName("profile", name); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.Host.Binding(name)
	before := int64(tenant.MaxProfileVersions + 1)
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListProfileVersions, binding, 1)
		if err != nil {
			return nil, "", err
		}
		n, err := strconv.ParseInt(f[0], 10, 32)
		if err != nil {
			return nil, "", fault.New(fault.Invalid, "the page token is not one this list made")
		}
		before = n
	}
	var out []tenant.ProfileVersion
	err := k.Host.Read(ctx, "the profile's versions cannot be listed", func(tx SendingTx) error {
		out = nil
		var one int
		found, err := tx.QueryRow(ctx, `SELECT 1 FROM profiles WHERE name = ?`, []any{name}, &one)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no profile named " + name)
		}
		return tx.Query(ctx, `SELECT `+profileColumns+` FROM profile_versions WHERE name = ? AND version < ?
			ORDER BY version DESC LIMIT ?`, []any{name, before, page.Limit + 1}, func(r Row) error {
			v, err := scanProfile(r, false)
			out = append(out, v)
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
	return out, EncodeCursor(ListProfileVersions, binding, strconv.Itoa(int(last.Version))), nil
}

// InstanceProfile implements tenant.Profiles.
func (k Deidentification) InstanceProfile(ctx context.Context, sopInstanceUID string) (tenant.ProfileRef, error) {
	if err := tenant.ValidateInstanceUID(sopInstanceUID); err != nil {
		return tenant.ProfileRef{}, err
	}
	var out tenant.ProfileRef
	err := k.Host.Read(ctx, "the instance's profile cannot be read", func(tx SendingTx) error {
		out = tenant.ProfileRef{}
		var name *string
		var version *int32
		found, err := tx.QueryRow(ctx, `SELECT p.name, p.version FROM instances AS i
			LEFT JOIN profile_versions AS p ON p.id = i.profile_version_id WHERE i.sop_instance_uid = ?`,
			[]any{sopInstanceUID}, &name, &version)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant holds no instance with this SOP Instance UID")
		}
		if name != nil && version != nil {
			out = tenant.ProfileRef{Name: *name, Version: *version}
		}
		return nil
	})
	return out, err
}

// ProfileVersionIDs returns the row id of each profile version the
// instances of a commit name, by ref: a version the tenant does not have
// is a precondition failure. A driver calls it in the commit's transaction
// and records each instance's id (NULL for the zero ref).
func ProfileVersionIDs(ctx context.Context, tx SendingTx, instances []tenant.ConfirmedInstance) (map[tenant.ProfileRef]int64, error) {
	ids := map[tenant.ProfileRef]int64{}
	for _, in := range instances {
		if in.Profile.IsZero() {
			continue
		}
		if _, done := ids[in.Profile]; done {
			continue
		}
		var id int64
		found, err := tx.QueryRow(ctx, `SELECT id FROM profile_versions WHERE name = ? AND version = ?`,
			[]any{in.Profile.Name, in.Profile.Version}, &id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, tenant.Precondition("the tenant has no profile version " + in.Profile.String())
		}
		ids[in.Profile] = id
	}
	return ids, nil
}

// LookupCode implements tenant.LookupTables.
func (k Deidentification) LookupCode(ctx context.Context, key tenant.LookupKey) (string, bool, error) {
	if err := key.Validate(); err != nil {
		return "", false, err
	}
	var code string
	var found bool
	err := k.Host.Read(ctx, "the lookup table cannot be read", func(tx SendingTx) error {
		var err error
		found, err = tx.QueryRow(ctx, `SELECT code FROM lookup_codes WHERE table_name = ? AND lookup_key = ?`,
			[]any{key.Table, key.Key}, &code)
		return err
	})
	if err != nil {
		return "", false, err
	}
	return code, found, nil
}

// AssignLookupCodes implements tenant.LookupTables. Each table's counter row
// is locked before any of its codes is read (SendingDialect.Lock), the
// tables in order of name, so two batches at once never wait for each other
// in a cycle and a table's codes are assigned one transaction at a time.
func (k Deidentification) AssignLookupCodes(ctx context.Context, keys []tenant.LookupKey, now time.Time) ([]string, error) {
	if err := tenant.ValidateLookupKeys(keys); err != nil {
		return nil, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return nil, err
	}
	var tables []string
	for _, key := range keys {
		if !slices.Contains(tables, key.Table) {
			tables = append(tables, key.Table)
		}
	}
	slices.Sort(tables)
	var out []string
	err := k.Host.Write(ctx, "the lookup codes cannot be assigned", func(tx SendingTx) error {
		out = make([]string, len(keys))
		last := map[string]int64{}
		for _, table := range tables {
			n, err := k.lockCounter(ctx, tx, table)
			if err != nil {
				return err
			}
			last[table] = n
		}
		given := map[tenant.LookupKey]string{}
		for i, key := range keys {
			if code, ok := given[key]; ok {
				out[i] = code
				continue
			}
			var code string
			found, err := tx.QueryRow(ctx, `SELECT code FROM lookup_codes WHERE table_name = ? AND lookup_key = ?`,
				[]any{key.Table, key.Key}, &code)
			if err != nil {
				return err
			}
			if !found {
				if code, err = k.nextCode(ctx, tx, key.Table, last); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO lookup_codes (table_name, lookup_key, code, assigned) VALUES (?, ?, ?, ?)`,
					key.Table, key.Key, code, k.D.Time(now)); err != nil {
					return err
				}
			}
			given[key] = code
			out[i] = code
		}
		for _, table := range tables {
			if _, err := tx.Exec(ctx, `UPDATE lookup_counters SET last_code = ? WHERE table_name = ?`, last[table], table); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nextCode returns the table's next code and counts it in last: one more
// than the most it has assigned, passing over a code the table holds
// already (one given to it otherwise than in order), and refusing a table
// whose codes would pass tenant.MaxLookupCodeLength digits.
func (k Deidentification) nextCode(ctx context.Context, tx SendingTx, table string, last map[string]int64) (string, error) {
	for {
		last[table]++
		code := tenant.LookupCodeText(last[table])
		if !tenant.ValidLookupCode(code) {
			return "", tenant.Precondition("the lookup table " + table + " has assigned every code it can")
		}
		taken, err := exists(ctx, tx, `SELECT 1 FROM lookup_codes WHERE table_name = ? AND code = ?`, table, code)
		if err != nil {
			return "", err
		}
		if !taken {
			return code, nil
		}
	}
}
