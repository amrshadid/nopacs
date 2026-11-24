// Package backup makes a backup of an installation and restores one: what
// the archive cannot make again from its storages (the master database and
// each tenant's, without the catalog), encrypted with the installation key
// file (package secret's backup cipher), and, at restore, the catalog
// rebuilt from the storages and the databases reconciled with what the
// storages say happened after the backup.
//
// # The plaintext
//
// Inside the cipher, a backup is lines of UTF-8 JSON, each ended by one
// "\n" (RFC 8259; RFC 7464's records without their separator byte): a
// header, then one section per database, the master first and then each
// tenant by name, then a footer. A line is an object with a "kind", but a
// row, which is an array. In order:
//
//	{"kind":"header","format":"nopacs-backup","version":1,"installation":ID,"time":T,"key_version":N,"sections":S}
//	{"kind":"section","index":I,"database":"master"|"tenant","tenant":NAME,"tenant_id":ID,"driver":DRIVER}
//	{"kind":"table","name":TABLE,"columns":[[NAME,KIND],...]}
//	[VALUE,...]                                   one per row of the table above
//	...                                           more tables, each with its rows
//	{"kind":"sequences","sequences":{TABLE:N,...}}
//	{"kind":"end","index":I,"tables":T,"rows":R,"point":POINT}
//	...                                           the next section
//	{"kind":"footer","sections":[{"index":I,"tenant":NAME,"tables":T,"rows":R,"sha256":HEX},...]}
//
// The master's section has "tenant", "tenant_id" and "driver" empty. T is
// a time in RFC 3339 with its fraction, in UTC. KIND is one of int, text,
// bytes, bool, time, float (package snapshot), and each VALUE is, by its
// column's kind: an integer; a string (or, for text that is not UTF-8, an
// object {"bytes": BASE64} of its bytes); BASE64 (RFC 4648 section 4, with
// padding); true or false; an integer of microseconds since
// 1970-01-01T00:00:00Z; a number; or null for NULL. "sequences" gives, per
// table whose ids the database makes, the highest id it ever gave, so a
// restore never gives one again.
//
// POINT is the section's recovery point, the moment it stands for:
//
//	{"snapshot_time":T,"schema_version":V,"schema":F,"event_history":H,"last_correction":C,"last_reception":R,
//	 "owners":[{"storage":S,"generation":G},...],"key_version":N}
//
// with event_history, last_correction, last_reception and owners empty for
// the master. F is the fingerprint of the baseline schema the database was
// made from (64 lowercase hexadecimal digits, driver.SchemaFingerprints)
// and V the migrations applied after it, the baseline being 1: a reader
// refuses a section whose F is not its own, or whose V it does not know. A section's SHA-256 is that of its bytes from the first byte
// of its "section" line to the "\n" that ends its "end" line; the footer
// lists every section's, and the file ends after the footer's "\n". A
// reader refuses a line out of this order, a count or a digest that does
// not match, a section missing or extra, and anything after the footer;
// the cipher has already refused a file cut short or changed.
// scripts/prove/backup_open.py reads the same layout with Python's standard
// library and checks every section's SHA-256.
//
// # Restore
//
// A restore runs with the server stopped, into an empty data directory: it
// reads the whole file once, authenticating every chunk, checking every
// section and every schema version, and writes nothing until that pass
// ends; then it takes the data directory's lock (the server's), writes its
// marker (MarkerPath), loads each database at its own schema version from
// the same open file, each section's digest checked before its load
// commits, and migrates it; suspends every key, whatever its row held; and
// reconciles each tenant with its storages: its quarantined files whose
// bytes were in incoming marked incoming lost, its storages' owner records
// taken at the next generation (one naming another owner, or a later
// generation, refused unless RestoreConfig.TakeOver), the correction
// records adopted (tenant.Restoring: a record is not authenticated, so it
// never moves a row the backup holds, and one with no row becomes an
// unresolved correction an admin decides), the catalog rebuilt from the
// storages through a rebuild job of replace mode (package rebuild; safe
// with nothing running), each record applied as its row says, which begins
// a new event history. The installation's id file, which the server needs
// to start, is written last and the marker removed after it, so a restore
// stopped half way leaves a data directory the server refuses, with the
// marker saying a restore was interrupted, rather than one with part of an
// archive.
//
// # What this package must never do
//
// It must never write a database, a storage or the data directory before
// the whole backup has been authenticated and checked; never print a
// secret, a key or an address; and never restore over a data directory or
// a database that holds anything.
package backup
