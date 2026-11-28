package api

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/deidjob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ChangeTrials tries a profile version over what a receiver last received
// (deidjob.Supervisor.TryChange). Config.SenderHealths may implement it, as
// it does SenderChanges; a server that does not answers a trial as
// unavailable.
type ChangeTrials interface {
	// TryChange runs the version over the first file of each scan of the
	// receiver's newest sessions and returns the counts.
	TryChange(ctx context.Context, tenantName string, t deidjob.ChangeTrial) (deidjob.ChangeTrialResult, error)
}

// detailForced and the details after it name, in the audit entry of a change
// of a receiver's profile, that the change was forced past change control.
const (
	detailForced      = "forced"
	detailForceReason = "force_reason"
	detailTrial       = "trial"
)

// TryReceiverProfile tries a profile version over what a receiver last
// received and records the result (receiver.proto, TryReceiverProfile).
func (s receiverService) TryReceiverProfile(ctx context.Context, req *v1.TryReceiverProfileRequest) (*v1.TryReceiverProfileResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReceiverServiceTryReceiverProfileProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("receiver", req.GetReceiver()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := profileName("profile", req.GetProfile()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetProfileVersion() < 0 {
		return nil, c.failIn(d, st, invalid("profile_version", "is 0 (the latest) or more"))
	}
	trials, ok := c.senders.(ChangeTrials)
	rec, rok := p.(tenant.ProfileTrials)
	if !ok || !rok {
		return nil, c.failIn(d, st, fault.New(fault.Unavailable, "this server tries no profile version over a receiver's receptions"))
	}
	if _, err := lookupReceiver(ctx, p, req.GetReceiver()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	profiles, err := profilesOf(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	v, err := readVersion(ctx, profiles, req.GetProfile(), req.GetProfileVersion())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	sessions, err := c.values().OnTenant(st).ChangeTrialSessions(ctx)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	got, err := trials.TryChange(ctx, req.GetTenant(), deidjob.ChangeTrial{Receiver: req.GetReceiver(), Profile: v.Ref(), Sessions: sessions})
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	now := c.now()
	facts := access.Facts{Object: req.GetReceiver(), Details: map[string]string{detailProfile: v.Ref().String(),
		"sessions": strconv.FormatInt(got.Sessions, 10), "files": strconv.FormatInt(got.Files, 10),
		"changed": strconv.FormatInt(got.Changed, 10), "held": strconv.FormatInt(got.Held, 10)}}
	err = rec.RecordProfileTrial(ctx, tenant.ProfileTrial{Receiver: req.GetReceiver(), Profile: v.Ref(), Tried: now, KeyID: callerKeyID(d),
		Sessions: got.Sessions, Files: got.Files, Changed: got.Changed, Held: got.Held}, changeEntry(d, facts), c.adminGuard(d))
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out := &v1.TryReceiverProfileResponse{Profile: v.Name, ProfileVersion: v.Version, Tried: timestamppb.New(now), Sessions: got.Sessions,
		Files: got.Files, Changed: got.Changed, Held: got.Held, Unchanged: got.Unchanged, Skipped: got.Skipped}
	for _, e := range got.Elements {
		out.Elements = append(out.Elements, &v1.ChangeElement{Tag: e.Tag, Action: e.Action, Files: e.Files})
	}
	for _, h := range got.Holds {
		out.Holds = append(out.Holds, &v1.ChangeHold{Reason: h.Reason, Files: h.Files})
	}
	return out, nil
}

// checkChangeControl judges a change of a receiver's profile against change
// control (changeControl): the receiver is pointed at another profile
// version, or given its first, only when that version was tried over its last
// receptions. A change that is no change of the receiver's profile, or that
// makes the receiver archive as received, is not judged, and may not carry a
// force.
func (c *core) checkChangeControl(ctx context.Context, p tenant.Pipeline, stored, result tenant.Receiver, profileChanged bool,
	req *v1.SetReceiverRequest) (access.Facts, error) {
	if !profileChanged || !result.Deidentifies() || result.Profile == stored.Profile {
		if req.GetForce() || req.GetForceReason() != "" {
			return access.Facts{}, invalid("force", "applies only to a change of the receiver's profile to a profile version")
		}
		return access.Facts{}, nil
	}
	return c.changeControl(ctx, p, result.Name, []tenant.ProfileRef{result.Profile}, req.GetForce(), req.GetForceReason())
}

// checkRuleSetChangeControl judges activating rs on the receiver rcv: each
// profile version an archive rule names that the receiver does not already
// run (its own profile, or an archive rule of the rule set it runs now) is
// pointed at, so it needs a trial as a change of the receiver's profile does.
// Without an activation a force is refused.
func (c *core) checkRuleSetChangeControl(ctx context.Context, p tenant.Pipeline, rcv tenant.Receiver, rs tenant.RuleSet, activating bool,
	force bool, reason string) (access.Facts, error) {
	if !activating {
		if force || reason != "" {
			return access.Facts{}, invalid("force", "applies only when a rule set is activated")
		}
		return access.Facts{}, nil
	}
	running := map[tenant.ProfileRef]bool{}
	if rcv.Deidentifies() {
		running[rcv.Profile] = true
	}
	if rcv.Routing.Version != 0 {
		cur, err := p.RuleSet(ctx, rcv.Name, rcv.Routing.Version)
		if err != nil && fault.KindOf(err) != fault.NotFound {
			return access.Facts{}, err
		}
		for _, a := range cur.Archive {
			running[a.Profile] = true
		}
	}
	var refs []tenant.ProfileRef
	for _, a := range rs.Archive {
		if !a.Profile.IsZero() && !running[a.Profile] && !slices.Contains(refs, a.Profile) {
			refs = append(refs, a.Profile)
		}
	}
	if len(refs) == 0 {
		if force || reason != "" {
			return access.Facts{}, invalid("force", "applies only to a rule set that names a profile version the receiver does not run")
		}
		return access.Facts{}, nil
	}
	return c.changeControl(ctx, p, rcv.Name, refs, force, reason)
}

// changeControl judges pointing receiver at each profile version of refs: a
// version is accepted only when it was tried for the receiver
// (TryReceiverProfile) with at least one file read and none held. Otherwise
// the change is a precondition failure naming the version and why, unless
// force is set with a reason, which makes it and returns the facts of the
// audit entry: that it was forced, why, and what each trial had found.
func (c *core) changeControl(ctx context.Context, p tenant.Pipeline, receiver string, refs []tenant.ProfileRef, force bool,
	reason string) (access.Facts, error) {
	rec, ok := p.(tenant.ProfileTrials)
	if !ok {
		return access.Facts{}, fault.New(fault.Internal, "api: the tenant's store keeps no profile trials")
	}
	var whys, names []string
	var refusal string
	for _, ref := range refs {
		trial, err := rec.ProfileTrial(ctx, receiver, ref)
		why := ""
		switch {
		case fault.KindOf(err) == fault.NotFound:
			why = "untried"
			refusal = "the profile version " + ref.String() + " was not tried over the receiver's last receptions: run TryReceiverProfile, or force the change with a reason"
		case err != nil:
			return access.Facts{}, err
		case trial.Files == 0:
			why = "no_files"
			refusal = "the trial of " + ref.String() + " over the receiver's last receptions read no file, so it shows nothing: try it once the receiver has received data, or force the change with a reason"
		case trial.Held > 0:
			why = "held=" + strconv.FormatInt(trial.Held, 10)
			refusal = "the trial of " + ref.String() + " over the receiver's last receptions found " + strconv.FormatInt(trial.Held, 10) +
				" files it would hold: fix the version, or force the change with a reason"
		}
		if why != "" {
			whys, names = append(whys, ref.String()+":"+why), append(names, ref.String())
		}
	}
	if len(whys) == 0 {
		return access.Facts{}, nil
	}
	if !force {
		return access.Facts{}, fault.New(fault.Precondition, refusal)
	}
	if tenant.ValidateUndoReason(reason) != nil {
		return access.Facts{}, invalid("force_reason", "is required with force: why, 1 to 1,000 characters")
	}
	return access.Facts{Details: map[string]string{detailForced: "true", detailForceReason: clip(reason, 200), detailTrial: clip(strings.Join(whys, ","), 200),
		detailProfile: clip(strings.Join(names, ","), 200)}}, nil
}
