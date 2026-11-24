package dicomweb

import (
	"net/http"
	"strconv"
	"strings"
)

// The texts of the Warning headers a search answers with (PS3.18 10.6.3.2,
// 8.3.4.4, 10.6.1.2.2).
const (
	// WarnFuzzy is the answer to fuzzymatching=true: the matching was
	// literal.
	WarnFuzzy = "The fuzzymatching parameter is not supported. Only literal matching has been performed."
	// WarnMore says more results match than the answer holds. The catalog
	// does not count matches, so the number left is not given.
	WarnMore = "There are additional results that can be requested"
	// WarnCapped says the limit asked for was more than one answer holds.
	WarnCapped = "The limit is more than the server returns at once; at most " + "1000" + " results are returned"
)

// Warning returns the value of a Warning header with the text (RFC 7234
// 5.5, PS3.18 8.3.4.4): code 299, the request's host as the agent ("-"
// when it has none), the text quoted with its quotes and backslashes
// escaped.
func Warning(r *http.Request, text string) string {
	agent := r.Host
	if agent == "" || strings.ContainsAny(agent, " \"") {
		agent = "-"
	}
	return "299 " + agent + " " + strconv.Quote(text)
}
