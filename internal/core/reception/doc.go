// Package reception is the life of a reception, the files of one study
// arriving at one receiver, as a state machine: its states, the one table
// of steps allowed between them, and the decision of when a reception that
// is receiving is complete.
//
// It is a package of its own because every part of the server that moves a
// reception (the receiver, the worker that archives it, the restart that
// takes up what a crash left) must agree on which step is allowed from
// which state. The database changes a reception's state by compare and set,
// and the step it sets is the one Step returns here, so no path through the
// code can take a step the table does not hold.
//
// # The states
//
//	receiving -> complete -> identified -> archived -> verified -> done
//	any state before done can end in quarantined or failed
//
// Done, quarantined and failed are ends: nothing leaves them. A file that
// arrives for a study whose reception is past receiving does not reopen it;
// it starts a new reception of the same study (a later arrival), which Step
// says by refusing the file with the kind Late. Running a quarantined or
// failed reception again, once its cause is fixed, is a new reception too,
// so a reception's history is never rewritten. A single bad file is
// quarantined on its own and the reception goes on with the others; the
// reception itself ends in quarantined only when nothing of it can go on.
//
// # Completion
//
// A reception is complete when it is quiet, not when it is whole: DICOM
// says nothing about how many files a study has, and the end of an
// association means nothing (a study may come over several, and one
// association may carry several studies). Complete decides from when the
// reception opened, the time of its last file, the quiet period, the
// longest time a reception may stay open, and now. The time is an argument,
// never read here, so the decision is a pure function a test pins down.
//
// # What this package must never do
//
// It must never do I/O, read the clock, or import anything of NoPACs'
// outside the core (.golangci.yml, depguard "core").
package reception
