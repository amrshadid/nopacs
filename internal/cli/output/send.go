package output

import (
	"fmt"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// SenderType is a sender's type as a word: "dimse", "stow", or "unknown" for a
// type this client was built before.
func SenderType(s *v1.Sender) string {
	switch s.GetType().(type) {
	case *v1.Sender_Dimse:
		return "dimse"
	case *v1.Sender_Stow:
		return "stow"
	}
	return "unknown"
}

// SenderWhere is where a sender sends to: host and port with the peer's AE
// title for a DIMSE sender, the URL for a stow sender.
func SenderWhere(s *v1.Sender) string {
	switch t := s.GetType().(type) {
	case *v1.Sender_Dimse:
		return fmt.Sprintf("%s:%d (AE title %s)", t.Dimse.GetHost(), t.Dimse.GetPort(), t.Dimse.GetCalledAeTitle())
	case *v1.Sender_Stow:
		return t.Stow.GetUrl()
	}
	return "-"
}

// SenderHealth says whether a sender reaches its peer now: "up", "unknown"
// before the first connection, or "down" with the reason, the fault and the
// server's detail. A state or reason this client does not know is shown as
// down for a reason it cannot name.
func SenderHealth(h *v1.SenderHealth) string {
	switch h.GetState() {
	case v1.SenderState_SENDER_STATE_UP:
		return "up"
	case v1.SenderState_SENDER_STATE_UNKNOWN:
		return "unknown (not connected yet)"
	case v1.SenderState_SENDER_STATE_DOWN:
		reason := "a reason this client cannot name"
		if _, known := v1.SenderDownReason_name[int32(h.GetDownReason())]; known && h.GetDownReason() != v1.SenderDownReason_SENDER_DOWN_REASON_UNSPECIFIED { // #nosec G115 -- an enum value
			reason = strings.ReplaceAll(enumWord(h.GetDownReason().String(), "SENDER_DOWN_REASON_"), "_", " ")
		}
		if h.GetFault() != v1.SenderFault_SENDER_FAULT_UNSPECIFIED {
			reason += ", " + SenderFault(h.GetFault())
		}
		if h.GetDetail() != "" {
			reason += ": " + h.GetDetail()
		}
		return "down (" + reason + ")"
	}
	return "unknown"
}

// SenderFault is why a connection to a peer failed, in words; "-" when there
// was no fault.
func SenderFault(f v1.SenderFault) string {
	if f == v1.SenderFault_SENDER_FAULT_UNSPECIFIED {
		return "-"
	}
	return strings.ReplaceAll(enumWord(f.String(), "SENDER_FAULT_"), "_", " ")
}

// SenderTestStep is a step of a sender test as a word.
func SenderTestStep(s v1.SenderTestStep) string {
	if s == v1.SenderTestStep_SENDER_TEST_STEP_UNSPECIFIED {
		return "unknown"
	}
	return enumWord(s.String(), "SENDER_TEST_STEP_")
}

// SenderTestOutcome is the outcome of a step of a sender test as a word.
func SenderTestOutcome(o v1.SenderTestOutcome) string {
	if o == v1.SenderTestOutcome_SENDER_TEST_OUTCOME_UNSPECIFIED {
		return "unknown"
	}
	return enumWord(o.String(), "SENDER_TEST_OUTCOME_")
}

// SenderTestSummary says in a sentence what a sender test found: the first
// step that failed with its fault and the peer's answer, or what the peer
// said at the last step that asked it something. dimse says whether the
// sender is a DIMSE one, whose last answer is a C-ECHO status and not an HTTP
// status.
func SenderTestSummary(results []*v1.SenderTestResult, dimse bool) string {
	for _, r := range results {
		if r.GetOutcome() != v1.SenderTestOutcome_SENDER_TEST_OUTCOME_FAILED {
			continue
		}
		s := "the peer was not reached: step " + SenderTestStep(r.GetStep()) + " failed"
		if r.GetFault() != v1.SenderFault_SENDER_FAULT_UNSPECIFIED {
			s += " (" + SenderFault(r.GetFault()) + ")"
		}
		if r.GetDetail() != "" {
			s += ": " + r.GetDetail()
		}
		return s
	}
	detail := ""
	for _, r := range results {
		if r.GetStep() == v1.SenderTestStep_SENDER_TEST_STEP_REQUEST && r.GetOutcome() == v1.SenderTestOutcome_SENDER_TEST_OUTCOME_PASSED {
			detail = r.GetDetail()
		}
	}
	if dimse {
		s := "the peer accepted the association and answered the C-ECHO"
		if detail != "" {
			s += " with status " + detail
		}
		return s
	}
	s := "the peer answered the request and takes the credential"
	if detail != "" {
		s += " (" + detail + ")"
	}
	return s
}

// JobState is a job's state as a word. A state this client does not know is
// "unknown".
func JobState(s v1.JobState) string {
	if s == v1.JobState_JOB_STATE_UNSPECIFIED {
		return "unknown"
	}
	if _, ok := v1.JobState_name[int32(s)]; !ok { // #nosec G115 -- an enum value
		return "unknown"
	}
	return enumWord(s.String(), "JOB_STATE_")
}

// JobEnded says whether a job's state is one it never leaves: done, partial,
// failed or canceled. RetryJob makes a new job; it never reopens one.
func JobEnded(s v1.JobState) bool {
	switch s {
	case v1.JobState_JOB_STATE_DONE, v1.JobState_JOB_STATE_PARTIAL, v1.JobState_JOB_STATE_FAILED, v1.JobState_JOB_STATE_CANCELED:
		return true
	}
	return false
}

// JobTarget is what a send job sends, as a path of labels: the subject, the
// session of it and the scan of that, as far as the job names them.
func JobTarget(s *v1.SendJob) string {
	parts := []string{s.GetSubjectLabel()}
	if s.GetSessionLabel() != "" {
		parts = append(parts, s.GetSessionLabel())
		if s.GetScanLabel() != "" {
			parts = append(parts, s.GetScanLabel())
		}
	}
	return strings.Join(parts, "/")
}

// JobDone is how many instances of a job have an answer: sent, sent with a
// warning, or refused.
func JobDone(c *v1.JobCounts) int64 {
	return c.GetInstancesSent() + c.GetInstancesWarned() + c.GetInstancesRefused()
}

// JobPercent is the share of a job's instances that have an answer, from 0 to
// 100. It is 0 for a job with no instance counted yet, which is a job still
// listing its scans, so the figure is not final until the counts say listed.
func JobPercent(c *v1.JobCounts) int {
	n := c.GetInstances()
	if n <= 0 {
		return 0
	}
	return int(min(JobDone(c)*100/n, 100))
}

// JobBar draws a job's progress as a bar of width cells inside brackets, plain
// ASCII so a terminal of any font draws it: "[=====>    ]".
func JobBar(c *v1.JobCounts, width int) string {
	inner := max(width-2, 1)
	filled := inner * JobPercent(c) / 100
	var b strings.Builder
	b.WriteByte('[')
	for i := range inner {
		switch {
		case i < filled:
			b.WriteByte('=')
		case i == filled && filled < inner && JobDone(c) > 0:
			b.WriteByte('>')
		default:
			b.WriteByte(' ')
		}
	}
	b.WriteByte(']')
	return b.String()
}

// JobProgress words a job's counts in one line: how many instances have an
// answer of those counted, with the warned and refused ones and the scans.
func JobProgress(c *v1.JobCounts) string { return jobProgress(c, "sent", "refused") }

// JobProgressOf is JobProgress in the words of the job's kind: a scrub's
// instances are read good or with a finding, a rebuild's cataloged or left
// out, and the others' sent or refused.
func JobProgressOf(j *v1.Job) string {
	sent, refused := JobCountWords(j)
	return jobProgress(j.GetCounts(), sent, refused)
}

// JobCountWords names the two outcomes of a job's instances as its kind
// means them, in lower case.
func JobCountWords(j *v1.Job) (sent, refused string) {
	switch j.GetKind().(type) {
	case *v1.Job_Scrub:
		return "read good", "with a finding"
	case *v1.Job_Rebuild:
		return "cataloged", "left out"
	case *v1.Job_Replicate:
		return "copied", "failed"
	case *v1.Job_Repair:
		return "repaired", "refused"
	case *v1.Job_Dispose:
		return "deleted", "failed"
	}
	return "sent", "refused"
}

// jobProgress words counts with the given names of the sent and refused
// outcomes.
func jobProgress(c *v1.JobCounts, sentWord, refusedWord string) string {
	s := fmt.Sprintf("%d of %d instances answered (%d%%): %d %s", JobDone(c), c.GetInstances(), JobPercent(c), c.GetInstancesSent(), sentWord)
	if c.GetInstancesWarned() > 0 {
		s += fmt.Sprintf(", %d with a warning", c.GetInstancesWarned())
	}
	if c.GetInstancesRefused() > 0 {
		s += fmt.Sprintf(", %d %s", c.GetInstancesRefused(), refusedWord)
	}
	s += fmt.Sprintf("; %d remaining", c.GetInstancesRemaining())
	if c.GetScans() > 0 {
		s += fmt.Sprintf("; scans %d of %d done", c.GetScansDone(), c.GetScans())
	}
	if !c.GetListed() {
		s += "; still listing the scans, so the total grows"
	}
	return s
}

// JobFailureCode is why an instance was not sent, in words.
func JobFailureCode(c v1.JobFailureCode) string {
	if c == v1.JobFailureCode_JOB_FAILURE_CODE_UNSPECIFIED {
		return "unknown"
	}
	return strings.ReplaceAll(enumWord(c.String(), "JOB_FAILURE_CODE_"), "_", " ")
}
