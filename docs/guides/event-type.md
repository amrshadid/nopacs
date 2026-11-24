# Adding an event type

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Testing](../testing.md) · Guides: [a database driver](database-driver.md) · [a storage driver](storage-driver.md) · [an API method](api-method.md) · [a migration](migration.md)

An event is written in the same transaction as the change it reports, and
read in commit order by the live feed.

1. **Define it in the tenant port**, `internal/tenant/event_payload.go`: an
   `EventType` constant, a payload struct with only labels, ids and counts
   (never a name, a UID or anything else a sender gave), its validation, and
   its case in `DecodeEvent`, so a reader of this version reads it and an
   older reader skips it.
2. **Write it where the change happens**, in both drivers, inside the change's
   transaction and after the feed's lock is taken (Postgres) so ids follow
   commit order: see `pipeline_events.go` in each.
3. **Add it to the contract**: a message in the `Event` oneof of
   `api/proto/nopacs/v1/monitor.proto`, a value of `EventType`, and its mapping
   in `internal/api/monitor_watch.go` (`eventMessage`).
4. **Test it**: the contract suite's event tests (`tenanttest/events*.go`) for
   both drivers, and a Watch case in `internal/api/monitor_test.go`.
5. **Show it**: the interactive program's tenant status follows events; say
   there what the new one changes, and count it in the proof of reception's
   events check when a reception writes it.
