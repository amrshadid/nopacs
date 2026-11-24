// Package dir is the directory storage driver: a blob.Store on a directory
// of a local disk or a network file system. A key is a path below the
// directory, "/" for each separator, so a person can read the archive as a
// tree and any tool can open an object as the file it is.
//
// # Whole or not at all
//
// Every Put writes its bytes to a file of its own in one temporary
// directory at the root, ".blob-tmp", and renames that file into place. A
// Put of "a/b/x" takes these steps, in this order:
//
//  1. Make every directory of the key durable, from the top: for "a",
//     make it if it is missing and sync the root; for "a/b", make it if it
//     is missing and sync "a". A new directory is an entry in its parent,
//     and an entry is durable only once its directory is synced: "Calling
//     fsync() does not necessarily ensure that the entry in the directory
//     containing the file has also reached disk. For that an explicit
//     fsync() on a file descriptor for the directory is also needed"
//     (Linux fsync(2)). The parent is synced whether this Put made the
//     directory or found it, since a directory found may have been made by
//     a Put whose sync never happened. The process keeps a bounded record
//     of the directories it has seen synced into their parents, and skips
//     those, so this costs a Put nothing in a directory already known
//     durable; after a restart the record is empty, and the first Put into
//     each directory pays its path's syncs once. A Put that finds another
//     Put of the process making the same directory durable waits for it to
//     finish rather than going on. So a Put returns only when every
//     directory on its key's path is known durable.
//  2. Create .blob-tmp/<random>, write the bytes to it, and sync it, so
//     the bytes are on the disk before any name points at them (the same
//     page: fsync "transfers all modified in-core data of the file").
//  3. Rename it to a/b/x. A rename replaces the name in one step: "a link
//     named new shall remain visible to other threads throughout the
//     renaming operation and refer either to the file referred to by new
//     or old before the operation began" (POSIX.1-2017, rename()); on
//     Windows Go renames with FILE_RENAME_REPLACE_IF_EXISTS and POSIX
//     semantics. A reader sees the old object or the new one, never a part
//     of either.
//  4. Sync a/b, which makes the new entry durable before Put returns.
//  5. Sync .blob-tmp. The rename changed two directories, and the
//     documentation promises durability only for a directory that was
//     synced, so both are. Without it, a crash could leave the temporary
//     name standing beside the object's on a file system that does not
//     journal renames as one change.
//
// A crash at any point leaves at worst a file in .blob-tmp, which no key
// can name: it is never listed, and Open removes every one it finds. Open
// reads that one directory and nothing else, so it costs the same for ten
// objects as for a hundred million; it never walks the tree.
//
// Open syncs the root and .blob-tmp once. A file system that cannot sync a
// directory (some network file systems answer EINVAL) cannot make a rename
// durable, and is refused there, saying so.
//
// # Nothing but the object is removed
//
// The driver removes nothing but the object a Delete names and its own
// files in .blob-tmp. It never removes a directory: not when a Delete
// empties one, not when a Put that made one fails. Removing a directory by
// name races any Put about to rename an object into it, and a removal by
// name cannot tell a directory from an object that took its name in
// between, so it could remove an acknowledged object. An empty directory
// costs one entry on the disk and nothing else: it holds no object, so it
// is never listed as a prefix.
//
// # One file system
//
// A rename cannot cross file systems (rename(2): EXDEV), so a storage's
// directory, its temporary directory included, must be one file system.
// Open checks the temporary directory against the root where the system
// can tell: by device number on Unix. On Windows the volume is not in what
// Go's Lstat returns, so it is not compared; a temporary directory that is
// a mount point or a junction is still refused, as not a plain directory. A
// volume mounted below the root cannot be found without walking the tree,
// so a Put into it fails at its rename, with an error that says a storage's
// directory must be one file system. The driver never copies instead,
// since a copy is not whole or nothing.
//
// # Staying inside the root
//
// Every file is reached through an os.Root, which refuses any path that
// would resolve outside the directory, through ".." or through a symbolic
// link, even one swapped in between a check and an open (go.dev/blog/osroot).
// On top of that the driver follows no symbolic link at all inside the
// root: a link where a directory of a key belongs fails the call, and a link
// where an object belongs is not an object. Keys are checked by
// blob.CheckKey before any of this, and hold only ASCII letters, digits,
// ".", "_" and "-", so a key never holds a separator, a device name, or a
// character a file system reads as another.
//
// A file or directory is opened first, without blocking (O_NONBLOCK on
// Unix), and what it is comes from the open descriptor, never from an
// earlier look at its name: a FIFO swapped in for an object cannot hang a
// Read, and is not read as one.
//
// # Listing
//
// Paging through a directory costs about one read of it. Its sorted names
// are kept in a small cache, at most 8 directories and 400,000 names in
// all; a directory with more names is listed in chunks instead, with a
// read per page and bounded memory. The process drops a directory's names
// on every change it makes in it, and a change by anything else is seen by
// the directory's modification time, except one that falls in the same
// tick of the file system's clock as the reading it follows.
//
// # What differs between systems
//
// The directory sync, the file system check, the errors for a full volume,
// a rename across file systems, a vanished directory and a file held open
// by another program, the non-blocking open, and the free space of the
// volume, each live in one small file per system (sys_*.go, vanish_*.go,
// usage_*.go). Everything else is the same code on Linux, macOS and
// Windows.
//
// On Windows a directory is not synced (see sys_windows.go). A rename or a
// removal that Windows refuses as access denied is tried again for about
// two seconds, since that is what Windows answers while a virus scanner or
// an indexer holds the file; a real permission error is therefore reported
// after that delay.
//
// # Limits
//
// One process owns a root at a time. Open removes every file in the
// temporary directory, so a second process opening the same root would
// remove the temporary files of the first's Puts in progress, and those
// Puts would fail. The server's lock on its data directory keeps a second
// server out; nothing here detects one.
//
// On a volume that ignores case (the default on macOS and Windows), two keys
// that differ only in case are one file, so the second Put replaces the
// first object. The port does not refuse such keys, because on a bucket and
// on most Linux volumes they are two objects, and refusing them would need
// a listing of every Put's directory. The archive's catalog refuses a label
// that differs from an existing one only in case, so its own layout never
// makes such a pair.
//
// One name cannot be a file and a directory at once, so a key cannot hold
// an object while other keys lie under it, or while a directory left by
// them remains: the Put is refused as Conflict.
//
// # What this package must never do
//
// It must never open, create or remove a file except through its os.Root,
// never build a path from anything but a key that passed blob.CheckKey or
// its own temporary directory, never follow a symbolic link, never remove
// a directory, never copy an object into place, and never let the root's
// path into an error. It never deletes an object except in Delete; what
// Open removes are files in its temporary directory.
package dir
