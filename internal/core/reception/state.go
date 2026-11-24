package reception

import "strconv"

// State is where a reception is in its life. Its text is what the database
// stores and logs show, so a value is never renamed.
type State string

// The states, in the order a reception passes through them, and the three
// ends.
const (
	// Receiving is a reception taking files.
	Receiving State = "receiving"
	// Complete is a reception that has been quiet for its quiet period, or
	// open for its longest time, and takes no more files.
	Complete State = "complete"
	// Identified is a reception whose files have their labels.
	Identified State = "identified"
	// Archived is a reception whose files, sidecars, manifests and catalog
	// rows are written.
	Archived State = "archived"
	// Verified is a reception whose archived files have been read back
	// and found to match the checksums taken at reception.
	Verified State = "verified"
	// Done is a reception with nothing left to do: an end.
	Done State = "done"
	// Quarantined is a reception none of whose files could go on: an end.
	Quarantined State = "quarantined"
	// Failed is a reception the server could not take further after its
	// retries, held for a person: an end.
	Failed State = "failed"
)

// States returns every state, in the order of the life of a reception and
// then the two ends other than done.
func States() []State {
	return []State{Receiving, Complete, Identified, Archived, Verified, Done, Quarantined, Failed}
}

// Event is what happened to a reception.
type Event string

// The events.
const (
	// File is a file received for the reception's study.
	File Event = "file"
	// Quiet is the decision of Completion that the reception is complete.
	Quiet Event = "quiet"
	// Identify is the labels of every file made.
	Identify Event = "identify"
	// Archive is every file and its records written.
	Archive Event = "archive"
	// Verify is every archived file read back and matched.
	Verify Event = "verify"
	// Finish is the last work after verification done: the incoming
	// copies removed and the reception's event recorded.
	Finish Event = "finish"
	// Quarantine is the decision that none of the reception's files can go
	// on.
	Quarantine Event = "quarantine"
	// Fail is a step that could not be done after its retries.
	Fail Event = "fail"
)

// Events returns every event.
func Events() []Event {
	return []Event{File, Quiet, Identify, Archive, Verify, Finish, Quarantine, Fail}
}

// steps is the one table of allowed steps: from a state, on an event, to a
// state. A pair that is not here is refused.
var steps = map[State]map[Event]State{
	Receiving:   {File: Receiving, Quiet: Complete, Quarantine: Quarantined, Fail: Failed},
	Complete:    {Identify: Identified, Quarantine: Quarantined, Fail: Failed},
	Identified:  {Archive: Archived, Quarantine: Quarantined, Fail: Failed},
	Archived:    {Verify: Verified, Quarantine: Quarantined, Fail: Failed},
	Verified:    {Finish: Done, Quarantine: Quarantined, Fail: Failed},
	Done:        {},
	Quarantined: {},
	Failed:      {},
}

// rank is a state's place on the main line, and an event's target's; it
// tells a step already taken from one not yet reached.
var rank = map[State]int{Receiving: 0, Complete: 1, Identified: 2, Archived: 3, Verified: 4, Done: 5}

// forward is the state each step of the main line leads to.
var forward = map[Event]State{Quiet: Complete, Identify: Identified, Archive: Archived, Verify: Verified, Finish: Done}

// Kind says why a step is refused, so a caller can act on it without
// reading the error's text.
type Kind int

// The kinds of refusal.
const (
	// Unknown is a state or an event this package does not know: a value
	// read from a database written by a later version, or a mistake.
	Unknown Kind = iota + 1
	// Late is a file for a reception past receiving: it starts a new
	// reception of the same study.
	Late
	// Ended is any other event for a reception that has ended.
	Ended
	// Already is a step the reception has already taken, as when a worker
	// runs a step again after a crash: the caller treats it as done.
	Already
	// OutOfOrder is a step the reception has not reached yet: a mistake of
	// the caller.
	OutOfOrder
)

// String returns the kind's name.
func (k Kind) String() string {
	switch k {
	case Unknown:
		return "unknown"
	case Late:
		return "late"
	case Ended:
		return "ended"
	case Already:
		return "already taken"
	case OutOfOrder:
		return "out of order"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Refusal is a step the table does not allow.
type Refusal struct {
	// State is the state the reception is in, which the refusal leaves
	// unchanged.
	State State
	// Event is the event refused.
	Event Event
	// Kind is why.
	Kind Kind
}

// Error returns the state, the event and the kind.
func (r *Refusal) Error() string {
	return "reception: " + strconv.Quote(string(r.Event)) + " in state " + strconv.Quote(string(r.State)) +
		" is refused: " + r.Kind.String()
}

// Step returns the state a reception in state s goes to on event e, or a
// *Refusal saying why it may not.
func Step(s State, e Event) (State, error) {
	from, okState := steps[s]
	if okState {
		if next, ok := from[e]; ok {
			return next, nil
		}
	}
	refuse := func(k Kind) (State, error) { return s, &Refusal{State: s, Event: e, Kind: k} }
	if !okState || !known(e) {
		return refuse(Unknown)
	}
	if e == File {
		return refuse(Late)
	}
	if len(from) == 0 {
		return refuse(Ended)
	}
	if to, ok := forward[e]; ok && rank[to] <= rank[s] {
		return refuse(Already)
	}
	return refuse(OutOfOrder)
}

// Ended reports whether s is an end: done, quarantined or failed.
func (s State) Ended() bool {
	from, ok := steps[s]
	return ok && len(from) == 0
}

func known(e Event) bool {
	for _, k := range Events() {
		if k == e {
			return true
		}
	}
	return false
}
