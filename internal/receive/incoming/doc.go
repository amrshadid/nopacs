// Package incoming is a tenant's incoming area: the directory on the server's
// own disk where a received file is kept, whole and synced, from the moment
// its sender is told it was received until its copy on a storage has been
// committed to the catalog and read back, or until a person decides about it.
//
// # Names are made here, and say how far a file got
//
// A file's name is an id this package draws at random, never a UID or
// anything else a sender gave, under the directory of the receiver it came
// to and a directory of the id's first two hex digits, so no directory grows
// past a few thousand names. The receiver is in the path because a restart
// that finds a file its database never heard of must know which receiver's
// rules to check it by. Every path is built from those parts and checked
// against the one form they make before it is used (ParsePath), and every
// file is reached through an os.Root, so no name can leave the area. The
// name also says where the file is in its life, which is what lets a
// restart tell a file still owed from one whose removal a crash interrupted:
//
//	<receiver>/ab/<id>.tmp   being written; never acknowledged; removed at start
//	<receiver>/ab/<id>.new   landed: whole and synced, its sender told it was
//	                         received, not yet recorded in the database
//	<receiver>/ab/<id>       recorded: a row of the tenant's database names it
//	<receiver>/ab/<id>.deid  made from the recorded file (its de-identified
//	                         copy, <id>.deid-<random>.tmp while one writer
//	                         writes it): never owed,
//	                         made again whenever needed, removed at start
//
// A file is renamed from .new to its plain name only after the row that
// names it has committed (Commit). So a plain name with no row is a file
// whose row was written and has since been let go (its reception verified
// or discarded, its quarantine record rejected): removing it loses nothing.
// A .new name with no row is a file its sender was told about and the
// database never heard of: it is checked and recorded again, never removed.
// The rename to the plain name is not synced: a crash that undoes it leaves
// a .new name with a row, which the restart renames again.
//
// # Landing: whole, synced, then acknowledged
//
// Land takes these steps, in this order, and only then returns:
//
//  1. Make the receiver's directory and the id's directory if they are
//     missing, syncing each one's parent, so their entries are durable before
//     any file is named in them. The process keeps the directories it has
//     made durable and skips them.
//  2. Create <receiver>/ab/<id>.tmp (exclusive, mode 0600) and copy the bytes into it
//     through SHA-256, at most the largest size plus one byte, so memory
//     does not grow with the file and a file over the bound is found without
//     reading it all. Over the bound, the file is removed and ErrTooLarge
//     returned: nothing of it is kept.
//  3. Sync the file, so its bytes are on the disk before a name points at
//     them (fsync(2): it "transfers all modified in-core data of the file").
//  4. Rename it to <receiver>/ab/<id>.new, which replaces the name in one step
//     (POSIX.1-2017, rename()).
//  5. Sync <receiver>/ab, so the new name is durable: a file's sync does not make its
//     directory entry durable (fsync(2): "an explicit fsync() on a file
//     descriptor for the directory is also needed").
//
// A crash before step 5 completes leaves at most a .tmp file, or a .new file
// whose sender was never answered and so will send it again; the restart
// handles both. On Windows a directory is not synced (sys_windows.go), as in
// the directory storage.
//
// # What this package must never do
//
// It must never build a path from anything but an id it drew and checked,
// never follow a symbolic link, never remove a file whose name is not one of
// its forms, never remove a directory, and never let the area's path
// into an error's text. It knows nothing of the database: which files are
// owed is the pipeline's to say (Scan lists, the caller decides).
package incoming
