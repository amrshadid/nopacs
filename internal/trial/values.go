package trial

import (
	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// HideValues sets each element's lengths (TrialElement.before_length and
// after_length) and, unless show, removes its values, so a trial's answer
// says what was done to each element without what it held. What a caller
// prints may leave the site, as an AI agent's output does, so a value is
// answered only when asked for (TryProfileRequest.show_values); the server
// and a trial run in the command line hide them alike.
func HideValues(results []*v1.TrialResult, show bool) {
	for _, r := range results {
		for _, el := range r.GetElements() {
			el.BeforeLength = int32(len(el.GetBefore())) //nolint:gosec // G115: a value is within AnswerBudget, far below 2 GiB.
			el.AfterLength = int32(len(el.GetAfter()))   //nolint:gosec // G115: as above.
			if !show {
				el.Before, el.After = "", ""
			}
		}
	}
}
