package api

import (
	"strconv"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/trial"
)

// The keys of a trial run's audit details.
const (
	detailChecksum  = "checksum"
	detailTried     = "files"
	detailRecords   = "quarantine_records"
	detailHeld      = "held"
	detailUndecided = "undecided"
	detailByRule    = "by_rule"
	detailByReason  = "by_reason"
)

// trialDetails returns the summary as an audit entry's details: the checksum
// tried, the counts, and the counts per rule and per reason written
// "key=count" by count, most first, cut at an entry's bound with how many
// more there were. Rule names and reasons are a profile's words and the
// server's, never a file's.
func trialDetails(s *trial.Summary, checksum string, records int) map[string]string {
	return map[string]string{
		detailChecksum: checksum, detailTried: strconv.Itoa(s.Files), detailRecords: strconv.Itoa(records),
		detailPassed: strconv.Itoa(s.Passed), detailHeld: strconv.Itoa(s.Held), detailUndecided: strconv.Itoa(s.Undecided),
		detailByRule: countsText(trial.Counts(s.ByRule)), detailByReason: countsText(trial.Counts(s.ByReason)),
	}
}

// countsText writes counts as "key=count,..." within an audit detail's
// bound, ending with "+N more" when they do not all fit.
func countsText(cs []*v1.TrialCount) string {
	var b strings.Builder
	for i, c := range cs {
		item := c.GetKey() + "=" + strconv.Itoa(int(c.GetCount()))
		more := "+" + strconv.Itoa(len(cs)-i) + " more"
		if b.Len()+len(item)+1+len(more)+1 > master.MaxAuditDetailValueLength {
			if b.Len() > 0 {
				b.WriteByte(',')
			}
			b.WriteString(more)
			break
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(item)
	}
	return b.String()
}
