// Package recall is the recall worker: it asks the stores of archive-tier
// storages to bring cold objects back, and polls each until it reads.
//
// A recall is a row of the tenant's database, one per object however many
// requests wait for it. The worker reads the rows due. A row requested has
// not been asked: the worker asks the store once (blob.Recaller) and records
// that the object reads, or that a restore is running, with the time to
// look again. A row in progress is looked at with Stat; once the object is
// online the row is recorded online with the time the store gave. An object
// found cold again while its row says in progress is asked for again, which
// a store answers with the restore it already runs, so no second restore is
// ever started.
//
// A store that is unreachable or overloaded leaves the recall due again
// after a wait that grows with each failure in a row. A store that refuses
// the recall (a speed it does not offer for the object's class), holds no
// such object, or cannot recall at all, fails it with a fixed reason: the
// store's own text never reaches a row, a log or an event.
package recall
