// Package deidjob carries out a tenant's deid jobs: it reads archived
// instances and judges each against a de-identification profile version,
// changing nothing, and records in the tenant's database what it found.
//
// # Who works what
//
// State lives in each tenant's database, never here. A deid job's work is
// units of the tenant's leased queue, all of the kind tenant.DeidWorkKind,
// listed and recorded through the same port as a send job's
// (tenant.JobWork): a listing unit lists the job's scans a page at a time,
// which queues one unit per scan; a scan's unit reads the scan's
// instances in the catalog's order after its cursor. A Supervisor reads the
// installation's tenants every Interval, and at once when poked (Poke),
// and works at most MaxUnits units at once in the whole server and at most
// MaxTenantUnits of one tenant, each claimed and worked to its end.
//
// # What a unit holds
//
// Each instance's bytes are read through package checkedread, every byte
// checked against the length and SHA-256 the catalog holds, into memory: at
// most MaxFileBytes; a larger file is not read, and passes with the note
// too_large. A read holds one of the server's read slots
// (checkedread.Slots, the one count the API's reads and the senders' are
// in) while it lasts; none free gives the unit back without spending an
// attempt. A dry run holds the file and its de-identified copy; bulk values
// (pixel data among them) stay in the file, and no copy of a value is made
// for an answer (receive.TrialOptions.NoValues). Measured on a 20 MiB file
// of 40 frames (TestDryRunMemoryOfAMultiFrameFile), it holds about 2.1
// times the file beyond the file itself, so about 3 times in all; a pixel
// rule adds the image it decodes, within the lower of the tenant's
// deid.pixel-limit and MaxPixelBytes. A dry run's copies are taken from
// the server's bound on de-identification's memory (Config.Memory,
// deid.memory), shared with the reception pipelines and the trial runs; a
// file whose copies pass the whole bound is reported as held,
// deid_failed/check_too_large, as a reception would hold it.
//
// # What a unit judges
//
// An archived file no longer holds the Patient ID a version that shifts
// dates keyed its shift by, so in both modes the engine identifies each
// file's subject by its scan's subject label from the catalog
// (engine.Reception.Subject), used as the shift's key alone and written
// nowhere: a dry run's keys are the unit's own, so the shift is
// throwaway, and a check compares no shifted value, only which elements
// remain.
//
// A rule's conditions read the instance's reception (receptionFacts): its
// receiver, its calling AE title when its files came with exactly one, and
// the called AE title of the receiver as configured now. A condition on an
// element of the file (Modality, SOP Class UID, Manufacturer, Model Name,
// Software Versions) reads the archived file, which still says what the
// file said only where the version kept the element.
//
// A dry run runs the job's profile version, or each instance's own when
// the job names none, under keys made for the unit (three random keys,
// never the tenant's, zeroed when the unit ends) with lookups answering
// every value unassigned, then the check after, through receive.TryFile
// with no receiver's checks.
//
// A check verifies each file against the version its instance records, by
// what the version, explained over the archived file (engine.Explain
// applies it to the data set it is given), would leave of each element the
// archive holds: removed is a finding (removed_element, or private_element
// for an odd group); emptied while the archive holds a value, kept_value;
// for an element removed or emptied unless the IOD requires it, anything
// but the archive's own value (the IOD's dummy, or nothing) is kept_value
// or removed_element, and the archive's value kept is none. Patient
// Identity Removed (0012,0062) must be YES and De-identification Method
// (0012,0063) must name the version (PS3.15 E.1.1). An element whose
// decision rests on a condition the archive cannot answer (a reception's
// fact not known, an element of the file the version removed or changed)
// is a note, check|undecidable, never a finding and never a silent pass.
//
// The check also looks up the file's UIDs outside the standard's registry:
// as they are, among the UIDs of what was archived as received, and
// remapped under the tenant's UID key, among every UID the catalog holds,
// which for a de-identified file are remapped. A hit is a UID some received
// file carried, unless the UID is itself one the catalog holds for a
// de-identified instance (the remapping made it; de-identified data sent
// back in). Only Study, Series and SOP Instance UIDs are recognized, the
// ones the catalog holds: an original Frame of Reference UID, or a
// Referenced UID of an instance never archived, is not. This lookup alone
// uses a tenant secret, the UID key, read alone (never the date or naming
// key) and zeroed when the unit ends; it answers whether a UID was found,
// never a pseudonym.
//
// # A re-run
//
// In the modes rerun and rerun_preview a unit runs a newer version over each
// archived file that an older version de-identified, with receive.TightenFile:
// only what the newer version decides otherwise than the older one, and only
// where it lets through less, is done to the archived file (what the older
// version did, a hash or a remapped UID or a shifted date, is never done
// again under other keys), and the check after runs over the result. The
// layer rule (profile.Stack, the older version under the newer) refuses a
// version that lets through more. A file the check holds, one that would need
// the original, and one a hold covers are refused with a fixed detail and
// never written; a file the version changes nothing in is skipped. A rerun
// hands each new file to a Reviser (Config.Revisions), which the server backs
// with the corrections' own edit path, one correction per instance, so
// superseded revisions, undo, purge, holds, records on every copy, audit and
// events are an edit's. A preview asks for none. Supervisor.TryChange runs the
// same judge over the first file of each scan of a receiver's newest sessions,
// for change control.
//
// # What is recorded
//
// The verdicts are recorded (tenant.JobWork.RecordJobProgress) in batches
// of at most MaxBatch instances or BatchInterval, each write fenced by the
// unit's attempt, with the counts per rule and per reason of the batch
// (JobProgress.Tallies): names of rules, actions, reasons, causes, tags and
// SOP classes, never a value read from a file. No log line, event or
// failure of this package carries a value read from a file either. A crash
// or a lost lease judges again at most one batch per unit; a cancel is seen
// at the next write; a stop records what was judged and gives the unit back
// without spending an attempt.
package deidjob
