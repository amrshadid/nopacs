package api

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The keys of the details of Recall's entries, beside detailBytes (the
// bytes asked for).
const (
	detailStorages = "storages"
	detailDays     = "days"
	detailObjects  = "objects"
)

// recallTokenPrefix starts every token a recall's preview gives, and
// recallMACLength is how many bytes of its HMAC-SHA-256 it keeps: 128
// bits, as RFC 2104 5 allows a truncated MAC, within the contract's 128
// bytes.
const (
	recallTokenPrefix = "r1"
	recallMACLength   = 16
)

// maxRecallTokenAge is how long a preview's token matches, the contract's
// hour.
const maxRecallTokenAge = time.Hour

// recalls returns st as the recalls' port.
func recalls(st tenant.Store) (tenant.Recalls, error) {
	r, ok := st.(tenant.Recalls)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no recalls")
	}
	return r, nil
}

// recallStorage is one archive-tier storage a recall asks, and what it
// holds of the target.
type recallStorage struct {
	name  string
	days  int
	speed tenant.RecallSpeed
	// bound is the storage's own recall_days: days above it need admin.
	bound int
}

// recallPlan is what a recall would ask now: the storages that hold any of
// the target and the target's objects there by what a recall does with
// them.
type recallPlan struct {
	storages             []recallStorage
	objects, bytes       int64
	online, recalling    int64
	askObjects, askBytes int64
	needsAdmin           bool
	path                 label.Path
}

// checkRecall returns the Invalid error of a recall request, and its
// target's path.
func checkRecall(req *v1.RecallRequest) (label.Path, error) {
	t := tenant.JobTarget{SubjectLabel: req.GetSubjectLabel(), SessionLabel: req.GetSessionLabel(), ScanLabel: req.GetScanLabel()}
	if req.GetSubjectLabel() == "" {
		return label.Path{}, invalid("subject_label", "is required")
	}
	if err := t.Validate(); err != nil {
		return label.Path{}, err
	}
	if req.GetStorage() != "" {
		if err := itemName("storage", req.GetStorage()); err != nil {
			return label.Path{}, err
		}
	}
	switch {
	case req.GetDays() < 0 || req.GetDays() > tenant.MaxRecallDays:
		return label.Path{}, invalid("days", "is 0 to 365; 0 for each storage's recall_days")
	case req.GetValidateOnly() && req.GetConfirm() != "":
		return label.Path{}, invalid("confirm", "is empty with validate_only")
	case !req.GetValidateOnly() && req.GetConfirm() == "":
		return label.Path{}, invalid("confirm", "is the confirm token of a preview (validate_only) of the same target, storage and days")
	case len(req.GetConfirm()) > tenant.MaxConfirmLength:
		return label.Path{}, invalid("confirm", "is at most 128 bytes")
	}
	return t.Path(), nil
}

// candidates returns the archive-tier storages a recall may ask: the one
// named, which must be of the archive tier, or every one of the tenant.
func candidates(ctx context.Context, p tenant.Pipeline, named string) ([]tenant.Storage, error) {
	if named != "" {
		s, err := lookupStorage(ctx, p, named)
		if err != nil {
			return nil, err
		}
		if !s.Tier.Archive() {
			return nil, precondition("the storage " + named + " is not of the archive tier; its objects read at once")
		}
		return []tenant.Storage{s}, nil
	}
	var out []tenant.Storage
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		items, next, err := p.ListStorages(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, s := range items {
			if s.Tier.Archive() {
				out = append(out, s)
			}
		}
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
}

// walkTarget calls fn with each page of the target's objects on storage
// and the recalls of those objects, in the store's order.
func walkTarget(ctx context.Context, rs tenant.Replication, rc tenant.Recalls, storage string, path label.Path,
	fn func(objs []tenant.StoredObject, rows map[string]tenant.Recall) error) error {
	page := master.Page{Limit: min(master.MaxPageLimit, tenant.MaxRecallBatch)}
	for {
		objs, next, err := rs.StorageObjects(ctx, tenant.StorageObjectQuery{Storage: storage, Path: path}, page)
		if err != nil {
			return err
		}
		keys := make([]string, len(objs))
		for i, o := range objs {
			keys[i] = o.Key
		}
		rows, err := rc.RecallsOf(ctx, storage, keys)
		if err != nil {
			return err
		}
		if err := fn(objs, rows); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}

// planRecall reads what a recall of path from the storages named (one, or
// every archive-tier one) for days (0 for each storage's own) would ask
// now, asking nothing of a store.
func planRecall(ctx context.Context, p tenant.Pipeline, rs tenant.Replication, rc tenant.Recalls, path label.Path, named string,
	days int, now time.Time) (recallPlan, error) {
	plan := recallPlan{path: path}
	stores, err := candidates(ctx, p, named)
	if err != nil {
		return plan, err
	}
	for _, s := range stores {
		bound := s.S3.RecallDays
		if bound == 0 {
			bound = tenant.DefaultRecallDays
		}
		rs1 := recallStorage{name: s.Name, days: days, speed: s.S3.RecallSpeed, bound: bound}
		if days == 0 {
			rs1.days = bound
		}
		var held int64
		err := walkTarget(ctx, rs, rc, s.Name, path, func(objs []tenant.StoredObject, rows map[string]tenant.Recall) error {
			for _, o := range objs {
				held++
				plan.objects++
				plan.bytes += o.Size
				r, ok := rows[o.Key]
				switch {
				case ok && r.Live():
					plan.recalling++
				case ok && r.Readable(now):
					plan.online++
				default:
					plan.askObjects++
					plan.askBytes += o.Size
				}
			}
			return nil
		})
		if err != nil {
			return plan, err
		}
		if held == 0 {
			if named != "" {
				return plan, precondition("the storage " + named + " holds none of the target in step")
			}
			continue
		}
		if rs1.days > bound {
			plan.needsAdmin = true
		}
		plan.storages = append(plan.storages, rs1)
	}
	if len(plan.storages) == 0 {
		return plan, precondition("no archive-tier storage holds any of the target in step")
	}
	return plan, nil
}

// binding is what a recall's token is bound to beside its counts: the
// tenant, the target, and each storage with its days.
func (pl recallPlan) binding(tenantName string) string {
	var b strings.Builder
	b.WriteString(tenantName + "\x00" + pl.path.Subject + "\x00" + pl.path.Session + "\x00" + pl.path.Scan)
	for _, s := range pl.storages {
		b.WriteString("\x00" + s.name + ":" + strconv.Itoa(s.days))
	}
	return b.String()
}

// names returns the plan's storages' names, in order of name.
func (pl recallPlan) names() []string {
	out := make([]string, len(pl.storages))
	for i, s := range pl.storages {
		out[i] = s.name
	}
	return out
}

// daysDetail returns each storage's days, as the entry records them.
func (pl recallPlan) daysDetail() string {
	parts := make([]string, len(pl.storages))
	for i, s := range pl.storages {
		parts[i] = s.name + ":" + strconv.Itoa(s.days)
	}
	return strings.Join(parts, ",")
}

// recallToken returns the token a preview of plan issued at issued gives:
// the issue time and the objects and bytes to ask in the clear, so a
// recall sent again after its first call asked for them answers what that
// call asked, and a MAC under the installation's confirm key over them and
// the plan's binding, so only this server's preview of the same target,
// storages and days makes one a recall takes.
func recallToken(key *secret.DerivedKey, tenantName string, plan recallPlan, issued time.Time) (string, error) {
	body := recallTokenPrefix + "." + strconv.FormatInt(issued.UnixMicro(), 10) + "." + strconv.FormatInt(plan.askObjects, 10) + "." +
		strconv.FormatInt(plan.askBytes, 10)
	mac, err := key.MAC([]byte(body + "\x00" + plan.binding(tenantName)))
	if err != nil {
		return "", err
	}
	return body + "." + base64.RawURLEncoding.EncodeToString(mac[:recallMACLength]), nil
}

// recallTokenParts is what a token holds in the clear.
type recallTokenParts struct {
	issued         time.Time
	objects, bytes int64
}

// readRecallToken returns what a token recallToken made for plan's binding
// holds, or a precondition failure for any other token or one more than an
// hour old at now.
func readRecallToken(key *secret.DerivedKey, tenantName string, plan recallPlan, token string, now time.Time) (recallTokenParts, error) {
	bad := precondition("the confirm token is not one this server's preview of a recall of this target, storage and days gave; preview again")
	f := strings.Split(token, ".")
	if len(f) != 5 || f[0] != recallTokenPrefix {
		return recallTokenParts{}, bad
	}
	micros, err1 := strconv.ParseInt(f[1], 10, 64)
	objects, err2 := strconv.ParseInt(f[2], 10, 64)
	bytes, err3 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || objects < 0 || bytes < 0 {
		return recallTokenParts{}, bad
	}
	mac, err := key.MAC([]byte(strings.Join(f[:4], ".") + "\x00" + plan.binding(tenantName)))
	if err != nil {
		return recallTokenParts{}, err
	}
	if !hmac.Equal([]byte(f[4]), []byte(base64.RawURLEncoding.EncodeToString(mac[:recallMACLength]))) {
		return recallTokenParts{}, bad
	}
	issued := time.UnixMicro(micros)
	if age := now.Sub(issued); age > maxRecallTokenAge || age < -time.Minute {
		return recallTokenParts{}, precondition("the confirm token is more than an hour old; preview again")
	}
	return recallTokenParts{issued: issued, objects: objects, bytes: bytes}, nil
}

// isTenantAdmin reports whether the caller holds admin on the decision's
// tenant, directly or through the installation.
func isTenantAdmin(d access.Decision) bool {
	ref, _ := d.Tenant()
	pr := d.Principal()
	return pr.Role(master.TenantScope(ref.ID)).Includes(master.RoleAdmin) || pr.Role(master.Installation).Includes(master.RoleAdmin)
}

// recallMessage returns a plan as the contract answers it.
func recallMessage(plan recallPlan, requested int64, confirm string) *v1.RecallResponse {
	return &v1.RecallResponse{Storages: plan.names(), Objects: plan.objects, Bytes: plan.bytes, ObjectsOnline: plan.online,
		ObjectsRecalling: plan.recalling, ObjectsRequested: requested, BytesRequested: plan.askBytes, Confirm: confirm}
}

// Recall previews a recall of a target's objects from archive-tier
// storages and gives its token, or asks the stores for what the preview of
// the token showed; an object online or being recalled already is never
// asked for again.
func (s replicaService) Recall(ctx context.Context, req *v1.RecallRequest) (*v1.RecallResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReplicaServiceRecallProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{detailValidateOnly: strconv.FormatBool(req.GetValidateOnly())}}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	path, err := checkRecall(req)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	q := readQuery{subject: path.Subject, session: path.Session, scan: path.Scan}
	if err := q.exists(ctx, p); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rs, err := replication(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rc, err := recalls(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	now := c.now()
	plan, err := planRecall(ctx, p, rs, rc, path, req.GetStorage(), int(req.GetDays()), now)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailStorages] = strings.Join(plan.names(), ",")
	facts.Details[detailDays] = plan.daysDetail()
	facts.Details[detailObjects] = strconv.FormatInt(plan.askObjects, 10)
	facts.Details[detailBytes] = strconv.FormatInt(plan.askBytes, 10)
	// The site set each storage's recall_days as a bound on what a recall
	// costs: days above it are an admin's to ask, preview or not.
	role := master.RoleOperator
	if plan.needsAdmin {
		if !isTenantAdmin(d) {
			return nil, c.failWith(d, st, facts, fault.New(fault.Denied, "days above a storage's recall_days need admin on the tenant"))
		}
		role = master.RoleAdmin
	}
	key, err := c.confirmKey()
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if req.GetValidateOnly() {
		token, err := recallToken(key, req.GetTenant(), plan, now)
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		c.record(d, st, master.OutcomeDone, "", facts)
		return recallMessage(plan, 0, token), nil
	}
	tok, err := readRecallToken(key, req.GetTenant(), plan, req.GetConfirm(), now)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	switch {
	case plan.askObjects == tok.objects && plan.askBytes == tok.bytes:
	case plan.askObjects == 0 && tok.objects > 0:
		// Every object the token's preview would ask is online or being
		// recalled now: the token's recall was made, and is answered again
		// as it asked, asking nothing more of the store.
		facts.Details[detailObjects] = strconv.FormatInt(tok.objects, 10)
		facts.Details[detailBytes] = strconv.FormatInt(tok.bytes, 10)
		c.record(d, st, master.OutcomeDone, "", facts)
		out := recallMessage(plan, tok.objects, "")
		out.BytesRequested = tok.bytes
		return out, nil
	default:
		return nil, c.failWith(d, st, facts, precondition("the recall would now ask for "+strconv.FormatInt(plan.askObjects, 10)+
			" objects ("+strconv.FormatInt(plan.askBytes, 10)+" bytes), not what the preview showed; preview again"))
	}
	if plan.askObjects == 0 {
		c.record(d, st, master.OutcomeDone, "", facts)
		return recallMessage(plan, 0, ""), nil
	}
	asked, audited, err := confirmRecall(ctx, rs, rc, plan, changeEntry(d, facts), c.roleGuard(d, role), now, func() {
		facts.Details[detailObjects], facts.Details[detailBytes] = "0", "0"
		c.record(d, st, master.OutcomeDone, "", facts)
	})
	switch {
	case err != nil && !audited:
		return nil, c.failWith(d, st, facts, err)
	case err != nil:
		// The entry went with the recalls asked before the failure.
		return nil, c.toAPI(d, err)
	}
	out := recallMessage(plan, asked.objects, "")
	out.BytesRequested = asked.bytes
	return out, nil
}

// confirmRecall asks for the plan's objects (askRecalls), and calls
// unasked, which records the call's entry, when every object became online
// or recalled by another request between the plan and the walk: the call
// then asked for nothing and wrote no entry with an ask, and a change is
// never left unrecorded. It answers what was asked and whether an entry was
// written.
func confirmRecall(ctx context.Context, rs tenant.Replication, rc tenant.Recalls, plan recallPlan, entry master.AuditEntry,
	guard tenant.Guard, now time.Time, unasked func()) (recallAsked, bool, error) {
	asked, audited, err := askRecalls(ctx, rs, rc, plan, entry, guard, now)
	if err == nil && !audited {
		unasked()
		audited = true
	}
	return asked, audited, err
}

// recallAsked is what askRecalls asked of the stores: the objects it named
// to RequestRecalls, those neither online nor being recalled at its own
// read of them, and their bytes. The store joins a row another request
// made meanwhile rather than ask twice.
type recallAsked struct {
	objects, bytes int64
}

// askRecalls records a recall of each object of the plan that is neither
// online nor being recalled, a page at a time, and answers what it asked
// for, and whether the entry was written. The entry goes with the first
// page: what it records is the plan, checked against the token. When no
// page has an object to ask for, no entry is written, and the caller
// records one.
func askRecalls(ctx context.Context, rs tenant.Replication, rc tenant.Recalls, plan recallPlan, entry master.AuditEntry,
	guard tenant.Guard, now time.Time) (recallAsked, bool, error) {
	var asked recallAsked
	audited := false
	for _, s := range plan.storages {
		err := walkTarget(ctx, rs, rc, s.name, plan.path, func(objs []tenant.StoredObject, rows map[string]tenant.Recall) error {
			var keys []string
			var size int64
			for _, o := range objs {
				if r, ok := rows[o.Key]; ok && (r.Live() || r.Readable(now)) {
					continue
				}
				keys = append(keys, o.Key)
				size += o.Size
			}
			if len(keys) == 0 {
				return nil
			}
			a := tenant.RecallAsk{Storage: s.name, Keys: keys, Days: s.days, Speed: s.speed, Now: now, Guard: guard}
			if !audited {
				a.Audit = &entry
			}
			if _, err := rc.RequestRecalls(ctx, a); err != nil {
				return err
			}
			audited = true
			asked.objects += int64(len(keys))
			asked.bytes += size
			return nil
		})
		if err != nil {
			return asked, audited, err
		}
	}
	return asked, audited, nil
}
