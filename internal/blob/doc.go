// Package blob is the storage port: the operations through which NoPACs
// puts, reads, lists and removes the bytes of a tenant's archive, whatever
// holds them. A storage is reached only through Store, so no code outside a
// driver knows whether its bytes are on a disk or in a bucket, and a new kind
// of storage is a new driver, not a change anywhere else.
//
// # What a driver promises
//
// Every promise is written on Store's methods and pinned by the contract
// suite in package blobtest, which every driver runs unchanged. The ones the
// archive cannot do without:
//
//   - Put is whole or not at all. An object that a reader can see holds all
//     the bytes of one Put, never part of one, and a key that already holds
//     an object holds either the old one or the new one, never a mixture,
//     whatever happens to the process or the Put midway. The size and the
//     SHA-256 Put returns are computed from the bytes as they pass, so the
//     checksum the catalog keeps is of what was written, not of a second
//     read.
//   - Memory does not grow with the size of an object or of a listing. An
//     object is a stream in and a stream out; a listing is a page at a time.
//   - A key is validated here, by CheckKey, before a driver uses it, and a
//     driver checks it again itself: a key that names nothing a storage can
//     hold safely, or that could leave the storage's root, is refused as
//     Invalid before anything is touched. The port carries the archive's
//     own keys, not arbitrary names: a segment is ASCII letters, digits,
//     ".", "_" and "-" only, which is what makes one key mean one object on
//     every storage and every file system (see CheckKey).
//   - Errors carry a kind from package fault: NotFound for a key that holds
//     nothing, Invalid for a key, prefix, range or page that is wrong as
//     written, Unavailable for a storage that cannot be reached or written
//     now.
//
// # What this package must never do
//
// It must never import a driver, a cloud SDK or anything that touches a file:
// it states the contract and validates keys, and nothing else. It never
// builds a key from a label; the archive's layout does that, in one place,
// and hands the result here. It never holds or logs a storage's
// credentials.
package blob
