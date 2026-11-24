# Routing a study by where it comes from

[Documentation index](../README.md) · [A reception, end to end](receiving.md) · [De-identification](../concepts/deidentification.md) · [The command line](command-line.md)

## How routes choose

A receiver's routes choose, for each study, the storage it is archived to, and
optionally a profile version that de-identifies it in place of the receiver's
and the senders it is forwarded to in place of the receiver's `--forward-to`.
A study is routed once, for its session: the routes are tried in order on the
first file of its first reception, as received (the first, by SOP Instance UID,
of its first series), and the first whose conditions all hold chooses. A
condition reads the calling or called AE title, or the file's Modality, SOP
Class UID, Manufacturer, Manufacturer's Model Name or Software Versions: never
a patient's name or identifier. It tests equals, starts with, a pattern (RE2,
the whole value) or present, and may be negated.

```sh
nopacs storage add lab1 lab-a --type dir --root fast
nopacs storage add lab1 lab-b --type dir --root fast
nopacs receiver route add lab1 scanner1 lab-a --storage lab-a --equals calling-aet=LAB_A
nopacs receiver route add lab1 scanner1 lab-b --storage lab-b --equals calling-aet=LAB_B \
    --profile strict@2 --forward-to pacs
nopacs receiver route default lab1 scanner1 quarantine
nopacs receiver route list lab1 scanner1
```

- **What is chosen is kept, for the session.** The route that chose, its
  profile version and its senders are kept with the reception when it is
  identified and with the session when its first scan is archived. Every later
  arrival of the session goes by that choice, from whatever AE title it comes;
  a restart, or a change of the routes afterwards, changes nothing for it. A
  reception of a study whose first reception is still being archived goes by
  the first's choice. Should two receptions of a study be routed apart all the
  same (two receivers at the same moment), the files of the second to archive
  are held as `unrouted` (`session_elsewhere`) before any is stored.
- **A route only replaces a profile.** Only a receiver that de-identifies may
  have a route that names a profile version, and the version must pass the
  same checks as a receiver's; a route can never turn de-identification off.
- **A study no route chooses** goes to the receiver's storage with its own
  profile and senders, or, after `route default ... quarantine`, every file of
  it is held as `unrouted`; release them naming the storage:
  `nopacs quarantine release lab1 q_7Zr2 --storage lab-a`. The release chooses
  the storage only: the routes (mended since, perhaps) still choose the
  profile and senders. A route that cannot be evaluated (one written by a
  later version of the server, or a value in the file longer than 1024 bytes)
  holds the study as `unrouted` too, whatever routes come after it.
- **Trials agree.** `nopacs profile try --receiver NAME` applies the receiver's
  routes: a file its route would de-identify with another version is tried
  with that version (the answer names the route and the version), and a file
  it would not route is shown held as `unrouted`. A held file (`--record`) of
  a study that already has a session is tried by the session's recorded
  choice, as its release would be, not by the routes as they are now. At a
  receiver that archives as received no route's version is used.
- **Limits.** 64 routes per receiver, 16 conditions per route, a pattern of 256
  bytes with no repetition above 100. A storage or a sender a route names
  cannot be removed while it names it.
- **Changes.** Each `route` command reads the receiver, changes its routes and
  writes them back only if nobody changed the receiver meanwhile; otherwise it
  is refused (exit 5) and changes nothing. In the API the routes are the
  receiver's `routing`, replaced whole by `SetReceiver` with the mask
  `routing`, and `expected_update_time` makes the same check. Changing needs
  admin; reading, reader. Every change is in the tenant's audit log.
- **Edit the routing as a file.** `nopacs receiver route export lab1 scanner1
  --to routes.json` writes the receiver's name, the time it was last changed
  (`updateTime`) and its whole routing (the API's `ReceiverRouting` as JSON);
  `nopacs receiver route import lab1 scanner1 routes.json` writes it back
  whole, only if the receiver was last changed at that time (exit 5 and no
  change otherwise: export again and redo the edit). A file without an
  `updateTime`, a file exported from another receiver, or
  `--ignore-update-time`, is written over the receiver as it is when read; a
  field the routing does not have is refused, never ignored.
