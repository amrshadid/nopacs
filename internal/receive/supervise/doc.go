// Package supervise makes what receives match what is configured: for every
// tenant it runs the tenant's reception pipeline and one listener per
// enabled receiver its database records, starts, stops and restarts them
// as the records change, and knows each receiver's health.
//
// # How it learns of a change
//
// State lives in each tenant's database, never here: a receiver runs because
// its row says so, and after a restart everything due is started from the
// rows alone. Each tenant has a worker that reads the tenant's receivers
// and makes the running listeners match them, every Interval as the
// backstop (30 seconds by default), and at once when it is poked: by Poke,
// which the API's handlers call after every receiver they add, change or
// remove (api.ReceiverChanges), and by Health, which the API calls with a
// receiver's row as stored (a row newer than what runs is a change to
// apply). A change the notice missed is seen at the next reading, within
// Interval. Space, the incoming limit and the certificates of running
// listeners (Checker) are looked at every SpaceInterval, however long
// Interval is.
//
// A listener that cannot start (its port taken, its TLS files unusable), or
// a running one whose Check fails (its certificate expired since it
// started), is shown down with its reason and tried again with a backoff that doubles
// from MinBackoff to MaxBackoff; the other receivers are not touched. A
// change of anything the listener is made from restarts it; a change of
// what only the pipeline reads (storage, naming rule, quiet period, longest
// open time) does not, since the pipeline reads the row itself.
//
// # What a receiver's health says
//
// Starting while its listener is being made; up when it admits callers;
// down with the contract's reason otherwise: disabled, its port or TLS
// files, the tenant's database not ready, the incoming disk or its storage
// below the reserve (storage.reserve), the tenant over its incoming limit
// (incoming.limit), or the server stopping. In the last four the port is
// held and new associations are rejected as transient, through the
// listener's Refusal, so a sender keeps its copy and tries again; the
// health says so once when it starts and once when it ends, as one event
// each (ReceiverHealthChanged), never per file or per association.
//
// While a rebuild that replaces the tenant's catalog is queued or running
// (tenant.Rebuilding's RebuildReplacing, read from the tenant's database
// at every round, so it holds across a restart and ends with the job
// however the job ends), the tenant's pipeline is stopped and none is
// started, and its receivers are down as not ready ("the tenant's catalog
// is being rebuilt"), refusing as transient: nothing is archived that the
// shadow being built would miss. Drained tells the rebuild when the stop
// is over.
//
// Callers turned away are grouped by receiver, failure and host (the
// contract's AssociationFailed): the first of a group makes an event at
// once, the rest in the minute after are counted into one event at its end,
// and hosts past 64 in a minute at one receiver fold into one event.
//
// # What it must never do
//
// It must never speak DICOM or import the package that does (it is handed a
// way to make a listener), read a file's bytes, keep a receiver running that
// its row no longer asks for past the next reading, let one tenant's slow
// database or one receiver's failure hold another's, or start a goroutine
// that Stop does not wait for.
package supervise
