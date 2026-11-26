# The security model

[Documentation index](../README.md) · [What an archive is made of](archive.md) · [Running NoPACs](../operate/running.md) · [De-identification](deidentification.md)

Who may do what, how that is recorded, and what the record can and cannot prove.
The commands are in [the command line](../use/command-line.md); the contract of
every method says which role it needs. NoPACs is built to meet the technical
requirements of HIPAA and GDPR (access control, audit, de-identification,
encryption of its own secrets and backups); it makes no legal claim, and whether a
deployment is compliant is its institution's to say.

## Keys

A caller over the network presents a **key**: `nopacs_<id>_<secret>`, in the
`Authorization: Bearer` header. The server keeps only the secret's SHA-256, so a
stolen database does not give a working key. A key is shown **once**, when it is
made. It has a public id, an expiry, and the name of the person or system that
holds it. Keys never appear in a log, an event, an error or an answer.

- A key can be limited to networks (`--network`) and to a TLS client certificate.
- A key is **rotated** (`nopacs key rotate <id> --overlap 24h`): a successor with
  the same grants and rules is made, and the old key keeps working for the
  overlap.
- A key is **revoked** (`nopacs key revoke`); it stops working on its next request.
  The server never caches a key or a grant.
- A key a restore *suspended* stays unusable until the local operator reinstates
  it ([backup](../operate/backup.md#keys-come-back-suspended)).

All of these are refused alike: no key, a wrong secret, an unknown id, an expired,
revoked or suspended key. The caller gets one answer, so it cannot tell which was
wrong.

## Grants and roles

A **grant** gives a key a **role** on one tenant or on the installation. A key with
no grant on a tenant is refused there, and a request that names a tenant that does
not exist is answered the same way as one without a grant, unless the caller may
list every tenant.

| Role | On a tenant | On the installation |
|---|---|---|
| **reader** | read its data, its status and its settings | list every tenant and read their states; read the installation's settings |
| **writer** | a reader that may also send data in | not given there |
| **operator** | a writer that may also run jobs and review quarantined files | a reader that may also see every tenant's status |
| **admin** | an operator that may also change the tenant's configuration, its settings and read its audit log | also create and remove tenants, make and revoke keys, set grants, change the installation's settings, and read its audit log |

Each role includes the ones below it, so one grant per scope is enough. The
contract names, on every method, the role it needs.

### What a tenant admin may grant

A key is one person's or system's identity across the installation, so only an admin on
the installation makes and revokes keys. Which keys may use a tenant is that tenant's
business, so a person holds one key and is let into each tenant by that tenant's admin. A
tenant's admin (an admin grant on that tenant, not on the installation) may:

- **set, change and remove grants on its own tenant**, at `reader`, `writer` or `operator`
  (`nopacs grant set <key-id> --role operator --tenant lab1`, `nopacs grant remove
  <key-id> --tenant lab1`);
- **list the grants on its tenant** (`nopacs grant list --tenant lab1`) and **the keys that
  hold one** (`nopacs key list --tenant lab1`), with each key's id and holder only.

A tenant's admin may **not**:

- grant or remove `admin`, on its tenant or any other: making or removing a tenant's admin
  stays with the installation's admin;
- change its own key's grant, or the grant of a key that is admin on the tenant;
- grant on any other tenant, or on the installation;
- touch a key's networks, expiry or other grants, make, rotate or revoke a key, or read a key's
  grants on other scopes;
- lift a retention rule's keep-at-least for a delete ([retention](../use/retention.md#keep-at-least-and-lifting-it)).

Each refusal is `PERMISSION_DENIED` with the rule in its message. Every grant change is
audited in the installation's log with the key's id, the scope and the previous and new role.
A tenant's admin may also set the tenant's own value of the settings marked so in
[the settings reference](../reference/settings.md).

## The local operator

A command on the server's local socket (`run/nopacs.sock` in the data directory)
carries no key: its caller is the operating system user that runs the server, the
**local operator**. That user is admin on the installation and on every tenant, and
is judged by no network rule. The socket is private to that user. This is why the
first key is made on the socket (`nopacs key create`, then `nopacs grant set`), and
why a few commands exist only for the local operator, such as reinstating a
restored installation's keys. Whoever can become that user owns the installation.

## Keys for AI agents: masked keys

An AI agent driven by an outside model sends whatever a command prints to its
vendor. Give such an agent, or any other outside service, a **masked key**
(`nopacs key create --holder "review agent" --masked`). A masked key keeps its role,
but the server never answers it a value that is not in its archived, de-identified
form, whatever the client: it is refused (`PERMISSION_DENIED`, exit 3) a
quarantined file's header, a search of quarantine or of receptions by a UID as
received, a trial's values, a trial or an explanation of a held file (`--record`:
which rule matched, a pattern's matches and a value's lengths would let it read a
held value back one guess at a time; it tries the files it sends), `deid code` and
lookup imports, an import's paths, the metadata and files of data archived as
received, and backups and exports; quarantine records and receptions reach it
without their UIDs as received.

A masked key also never chooses where archived data goes: a send job, attaching or
filling a replica, adding or changing a storage or a sender, and a receiver made to
forward (`forward_to` or a route's senders) are refused to it (`PERMISSION_DENIED`,
"a masked key may not choose where archived data goes; a person does"), each refusal
in the audit log. Where data goes is a person's choice.

The flag is kept by every rotation and can never be removed (make a new key
instead), a masked key can make and rotate only masked keys (a plain key's
successor would hand it a plain secret), and every audit entry it makes carries the
detail `masked` = `true`, so the log shows what an agent read.

What a person writes into a profile (a rule's name, a pattern, a replacement value)
is answered as written by `profile get`, `diff` and `describe`, to a masked key too:
a rule that names a patient is that person's doing, so keep identifiers out of
profile documents.

Previews hide values from every key by default: `profile try` and
`quarantine show --header` show each element's tag, VR, length, rule, action and
whether it changed, but no value, unless `--show-values` asks; the server records
that in the audit log. `nopacs agent-guide` prints every command and the shape of
its `--json`, for an agent to read.

What stays the installation's choice: de-identified data can still point to a
person (dates, rare findings, free text a profile keeps). Which model may see which
tenant, and under which agreement with its vendor, is decided by the site.

## Networks and limits

Settings say where requests may come from (`access.allowed-networks`,
`access.denied-networks`, `access.tenant-networks`, `access.trusted-proxies`) and
how a caller that keeps failing is slowed and then blocked
(`access.failure-limit`, `access.failure-window`, `access.block-time`); see
[the settings](../reference/settings.md). A receiver is closed by default too: it
admits only the callers it lists, and "open to all" is something a person writes
(`--any-caller`).

## Audit and sealing

Every request is recorded: who (which key), what, when, from where, and the outcome
(done, refused or failed). Which log it goes to: a refused request, always the
installation's; an allowed request, the log of the tenant or the installation it
concerns. A **change and its entry are written together or not at all**. The entry
of a *read* or a *refusal* is written in batches, and when the server is
overwhelmed it is dropped, counted and reported, never blocking the request
(`nopacs_audit_entries_dropped_*` in [the metrics](../reference/metrics.md)).

`nopacs audit list` reads the log. **Sealing** makes it tamper-evident: every
`audit.seal-interval` the server gives each entry its place in a hash chain
(SHA-256) and signs a checkpoint with Ed25519, using a key derived from the
installation's key file and never stored. `nopacs audit verify` recomputes the chain
and checks each signature; `nopacs audit export` writes the log as JSON lines that
standard tools can check without the server.

What sealing shows, and what it cannot: it shows that an entry was removed or
changed after it was sealed. An entry not yet sealed (at most one seal interval) can
be removed without trace. Someone who holds the key file *and* can write every
database can seal a rewritten log again, and a rollback of every database together
is consistent. Only a public key, a sequence number or a digest **kept outside the
installation** shows those: save `nopacs audit public-key` and the digest `export`
prints. See [running](../operate/running.md#check-that-the-audit-log-was-not-changed).

## Secrets at rest

Secrets the installation holds (a tenant database's address, an S3 credential, the
naming, UID and date-shift secrets of a tenant) are sealed with the installation's
key file. A backup and a tenant export are encrypted. **Image data is not encrypted
by NoPACs**: the storage does that (an encrypted disk or volume, S3 server-side
encryption). Keep the key file apart from the backups.

No flag takes a secret: a key comes from `NOPACS_KEY` or `--key-file`, a database
address from a variable (`--db-env`, `--master-env`), a storage credential from
`--credential-env` or `--credential-file`.

## Input is hostile

Sizes are bounded; names used in paths are checked before they reach a storage; a
DICOM file is never trusted to be well formed, and one that is not goes to
quarantine. A de-identifying receiver fails closed: a file the profile cannot
process, or in which a removed value is found again, is held and never archived as
it came ([de-identification](deidentification.md)).

## What is weak

- Whoever holds the local operator's account, or the key file and the databases,
  holds everything. The audit seal is checked from outside to be sure.
- A key's network and certificate limits are only as good as the network facts the
  server sees; behind a proxy, `access.trusted-proxies` must be exactly right.
- Entries of reads and refusals can be lost under load or a kill within the flush
  interval; the loss is counted.
