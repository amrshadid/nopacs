package output

import (
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// Quota words what a tenant holds against its storage quota, for a status
// or a tenant's details: its bytes and instances, each against its bound or
// "no bound", and where that stands. It answers "" when the server sent no
// quota (the tenant's database did not answer), so a caller shows a line
// only when there is something to say. The commands and the interactive
// program share it, so a quota is never worded two ways.
func Quota(q *v1.TenantQuota) string {
	if q == nil {
		return ""
	}
	bytes := Human(q.GetUsedBytes()) + " of " + bound(q.GetBytesBound(), Human)
	files := strconv.FormatInt(q.GetUsedInstances(), 10) + " instances of " +
		bound(q.GetInstancesBound(), func(n int64) string { return strconv.FormatInt(n, 10) })
	return bytes + ", " + files + " (" + QuotaState(q.GetState()) + ")"
}

// QuotaPurge words the part of a tenant's use that its purges will give
// back, or "" when there is none.
func QuotaPurge(q *v1.TenantQuota) string {
	if q.GetPurgeInstances() == 0 && q.GetPurgeBytes() == 0 {
		return ""
	}
	return Human(q.GetPurgeBytes()) + " in " + strconv.FormatInt(q.GetPurgeInstances(), 10) + " files awaiting purge"
}

// QuotaWarning words the warning a tenant's quota stands under, or "" when
// it is under its warning or has no bound.
func QuotaWarning(q *v1.TenantQuota) string {
	switch q.GetState() {
	case v1.QuotaState_QUOTA_STATE_REACHED:
		return "WARNING: the tenant is at its storage quota; new files are refused until a purge or a raised bound makes room"
	case v1.QuotaState_QUOTA_STATE_WARNED:
		return "WARNING: the tenant is past quota.warn (" + strconv.Itoa(int(q.GetWarnPercent())) + "%) of its storage quota"
	}
	return ""
}

// QuotaState names a quota's state in a word.
func QuotaState(s v1.QuotaState) string {
	switch s {
	case v1.QuotaState_QUOTA_STATE_UNDER:
		return "under"
	case v1.QuotaState_QUOTA_STATE_WARNED:
		return "warned"
	case v1.QuotaState_QUOTA_STATE_REACHED:
		return "reached"
	}
	return "unknown"
}

// bound writes a bound with format, or "no bound" for 0.
func bound(n int64, format func(int64) string) string {
	if n == 0 {
		return "no bound"
	}
	return format(n)
}
