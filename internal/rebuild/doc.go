// Package rebuild runs a tenant's rebuild jobs: the catalog made again from
// what the tenant's storages hold, with no help from the catalog it
// compares with or replaces.
//
// A rebuild reads, on each storage in order of name, the correction records
// under meta/.corrections/ first (layout.CorrectionsPrefix, package
// record), then lists meta/ and reads each scan's manifest (package
// manifest) and metadata (package sidecar, version 3: the scan's header and
// each instance's line); it opens no DICOM file. What it reads goes into a
// shadow catalog, scan by scan, each write fenced by the job's unit and
// moving the build's cursor in the same transaction
// (tenant.Rebuilding.RebuildScan), so a crash or a lost lease goes on from
// the last scan written and never touches the catalog in use. Once every
// storage is read, a compare counts every difference from the catalog in
// use and drops the shadow; a replace makes the shadow the catalog in use
// in one transaction (tenant.Rebuilding.ActivateCatalog).
//
// What a rebuild leaves out it counts, never guesses:
//
//   - a scan a delete's correction record names, and the instances an
//     instance delete names (tenant.TallyLeftOutDeleted). With no
//     Config.Decide a record is applied as its state says, a delete still
//     prepared as committed: a delete is fail-closed, since showing
//     withdrawn data is the worse error. The server's rebuilds and a
//     restore's apply each record as the correction's row says (ByRow):
//     a delete prepared, undone or abandoned there hides nothing, whatever
//     its record says. Each delete applied is tallied unresolved
//     (tenant.TallyUnresolvedCorrection). A relabel's record hides the tree its state
//     says is not the archive's (the old labels once published, the new
//     ones before), so the two trees a relabel leaves during its grace are
//     never both cataloged;
//   - a scan whose metadata names a profile version the tenant does not
//     have (tenant.TallyLeftOutProfileUnknown), never cataloged as
//     archived as received;
//   - a scan whose manifest or metadata cannot be read, is of a form older
//     than version 3, or says another thing than the other
//     (tenant.TallyLeftOutUnreadable);
//   - a scan the shadow cannot hold beside one already in it: its labels
//     held by another (tenant.TallyLeftOutLabelConflict), its series or an
//     instance held by a scan of another storage
//     (tenant.TallySeriesOnTwoStorages).
//
// A correction record that cannot be read stops the build (the unit fails,
// and is tried again): what it would hide cannot be known, and a rebuild
// that showed it anyway would undo a withdrawal.
//
// Nothing is written to a storage: every storage a unit holds is typed as
// Storage, which reads and lists only.
package rebuild
