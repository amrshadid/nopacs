# Running NoPACs

[Documentation index](../README.md) · [A first archive](../start/first-archive.md) · [The command line](../use/command-line.md) · [Settings](settings.md) · [Backup](backup.md)

For someone who installs and operates an installation. The commands are in
[the command line](../use/command-line.md); this page is the order to use them in and
what the server needs. `init` and `serve` are commands of `nopacs-server`, the
server's program; the rest of the commands are `nopacs`, the command line.

## Make an installation

```sh
nopacs-server init --data-dir /var/lib/nopacs
```

`init` makes the data directory, the **key file** (`key`) and the master
database (`master.db`, SQLite), and prints where the key file is. The key file
seals every secret the installation keeps; losing it loses them all, so copy it
somewhere only you can read. `init` refuses a directory that already holds an
installation. The data directory also holds `tenants/` (a SQLite file per tenant),
the incoming area, and `run/` (the local socket `nopacs.sock` and the lock that
keeps a second server away).

To keep the master database in Postgres instead, name an environment variable
that holds its address: `MASTER_DB='postgres://...' nopacs-server init --data-dir ...
--master-env MASTER_DB`, and pass the same `--master-env` to `serve`. An address
is never an argument.

## Run the server

```sh
nopacs-server serve --data-dir /var/lib/nopacs
nopacs-server serve --data-dir /var/lib/nopacs --listen 0.0.0.0:8443 --tls-cert cert.pem --tls-key key.pem
```

Without `--listen` the server answers only on the local socket. Its flags are
`--listen`, `--tls-cert` and `--tls-key` (a renewed certificate in the same files
is picked up without a restart), `--master-env`, `--log-format json|text`,
`--log-level`, and `--drain` (how long requests in flight may run after a stop
signal, 15 seconds by default). Logs and metrics (`/metrics`) are described in
[obs](../../internal/obs/doc.go). Which changes need a restart is in
[Settings](settings.md#which-changes-take-effect-without-a-restart).

The master database and each tenant's live in SQLite files in the data directory
or in Postgres. One process writes a SQLite database: the server's lock on its
data directory keeps a second server away, a second process on the same file
would be safe for the data but could wait for as long as the first keeps writing
and then fail as busy, and Postgres is the database for anything that needs
several writing processes.

## The first key

On the local socket the commands need no key: they run as the user that runs the
server. Make the first key there and give it a role, then use it over the network:

```sh
nopacs key create --holder "operations" --data-dir /var/lib/nopacs      # shown once
nopacs grant set <key-id> --role admin --installation --data-dir /var/lib/nopacs
export NOPACS_KEY=nopacs_...                                              # on the client
nopacs whoami --server https://archive.example.org:8443
```

Keys, roles, the networks a key may come from, and the audit log are explained in
[the security model](../concepts/security.md), and in the comments of
[access](../../internal/access/doc.go). The roles are reader, writer, operator and
admin, on a tenant or on the installation.

### Name the servers and keys you use

A person who works with several installations, or with a key per tenant, saves
each as a context instead of repeating flags. `nopacs context add` sends the key
to the server first, saves only if the server accepts it, and prints who the key
is and what it is granted:

```sh
nopacs context add prod --server https://archive.example.org:8443 --key-file ~/.nopacs/prod.key --ca-file ~/.nopacs/ca.pem
nopacs context add lab1 --server https://archive.example.org:8443 --key-file ~/.nopacs/lab1.key --ca-file ~/.nopacs/ca.pem --tenant lab1
nopacs --context lab1 setting list        # the tenant lab1, from the context's default tenant
nopacs context use prod                   # the context commands use when none is named
```

The contexts are in a file only you can read, which holds the paths of the key
and certificate files and never a key; see
[Contexts](../use/command-line.md#contexts).

### Check that the audit log was not changed

Every request writes an entry to the audit log of its scope. The server seals
every log every `audit.seal-interval` (ten minutes) and once more when it
stops: each entry gets its seq, its place in a hash chain (SHA-256, FIPS
180-4), and each pass ends in a checkpoint signed with Ed25519 (RFC 8032) by a
key derived from the installation key file, never stored. The service is
`AuditSealService` ([audit_seal.proto](../../api/proto/nopacs/v1/audit_seal.proto)):
verify, checkpoints and export need the admin role on the log's scope; the
public key, any caller the server authenticates.

```sh
nopacs audit public-key > audit-key-1.pem      # keep it outside the installation
nopacs audit verify                            # the installation's log
nopacs audit verify --tenant lab1 --expect-seq 120344
nopacs audit checkpoints --limit 5
nopacs audit export --tenant lab1 --out lab1-audit.jsonl
python3 scripts/prove/audit_verify.py lab1-audit.jsonl --public-key 1=audit-key-1.pem
```

`verify` recomputes the chain and checks each checkpoint's signature with the
public keys of the key file, the anchors the installation's log and each
tenant's keep of each other, and the values you kept outside the installation:
`--expect-seq` (the last sealed seq) and `--expect-digest` (a checkpoint's
digest, which `export` prints). A log with a problem is listed with each
problem's kind and seq, and the command exits 1. A log whose unsealed entries
waited more than two seal intervals is reported as stalled, with the time of the
oldest. One call of the server verifies at most a million entries or ten
thousand checkpoints, and stops early when less than two seconds of its
deadline are left; `verify` calls again from where each call stopped until the
range is done. `export` writes the log as JSON lines that hashlib and openssl
check without the server; `scripts/prove/audit_verify.py` is such a checker,
given the public key you kept, never the one in the file. With `--out` it
writes a new temporary beside the file, readable by its owner alone, and gives
it the file's name only when the export is whole, never replacing a file that
is there or that appeared meanwhile. What a seal cannot show:
someone who holds the key file and can write every database can seal a
rewritten log again, and a rollback of every database together is consistent;
only a public key, a seq or a digest kept outside the installation shows those.
The keys answered are this installation's: a tenant moved in from another
installation is not checked against the keys of the installation it came from
([backup](backup.md#what-stays-weak) says why), so `audit public-key
--installation` with another installation's id answers not found (exit 4). Keep
the source's public key before a tenant is moved.

## Make a tenant that receives

The installation says where data and receivers may be; the tenant then uses them
(the same steps, as API calls, are in
[Receiving through the API](../api/reception-walkthrough.md)).

```sh
nopacs tenant create lab1
nopacs setting set storage.directory-roots fast=/srv/archive
nopacs setting set receiver.ports 11112 --tenant lab1
nopacs setting set receiver.allow-as-received true --tenant lab1   # test data only, see below
nopacs storage add lab1 main --type dir --root fast --path main
nopacs storage test lab1 main
nopacs receiver add lab1 scanner1 --storage main --port 11112 --aet NOPACS \
    --allow-aet PRISMA1 --allow-from 10.20.0.0/24 --enabled
```

A tenant on Postgres is made with `--db-env VAR` or `--db-file PATH`. An S3
storage is added with `--type s3` and an endpoint the setting
`storage.s3-endpoints` lists. A receiver admits only callers it lists; open to
everyone is something you write down (`--any-caller`). A STOW-RS receiver
(`--type stow --allow-key ID`) takes HTTP requests from the keys it lists.

From here: send a study ([a reception, end to end](../use/receiving.md)), look at what
was archived with `nopacs catalog subjects lab1` and `nopacs download`
([reading the archive](../use/reading.md)), and follow a tenant with the interactive
program (`nopacs`).

## Send to another archive

A tenant sends what it holds to a PACS or a DICOMweb service through a sender,
to a destination the installation lists ([Sending](../use/sending.md) has the steps):

```sh
nopacs setting set sender.destinations "pacs.lab.example ports=104"
nopacs sender add lab1 pacs --type dimse --host pacs.lab.example --port 104 \
    --called-aet ORTHANC --calling-aet NOPACS --enabled
nopacs sender test lab1 pacs
nopacs job create lab1 --sender pacs sub-0017 --wait
nopacs tenant status lab1
```

- **TLS files.** A sender's CA file, client certificate and key are named by
  file name only, and read from `<data-dir>/tls/<tenant>/`, the directory a
  receiver's TLS files are in, so a remote admin cannot point TLS at files
  nobody placed there. Put them there (readable by the server's user only)
  before naming them with `--tls-ca-file`, `--tls-certificate-file` and
  `--tls-private-key-file`.
- **A stop gives the work back.** At a stop signal each sender finishes the
  instance it is sending, records what the peer answered, and gives its unit
  back without spending an attempt; the next start takes it up from there,
  sending again at most one batch (64 instances) per unit. The senders are
  stopped with the receivers and the pipelines, before the API's requests are
  drained, within their own bound of 90 seconds: `--drain` bounds only the
  API's requests in flight, not the sends. A second signal exits at once, and
  a unit cut short is taken up again once its lease runs out.
- **Where to look.** `nopacs tenant status lab1` shows each sender's health and
  the jobs in progress; `nopacs watch lab1 --type job-step --type sender-health`
  follows them as they change.

## What next

- Choose what each receiver does with what it receives, and make a profile:
  [de-identification](../concepts/deidentification.md) and
  [profiles](../use/profiles.md).
- Send each study to a storage by where it comes from:
  [routing](../use/routing.md).
- Import a directory of files that already exist: [importing](../use/importing.md).
- Back the installation up, and move a tenant: [backup](backup.md).
- When something does not work: [troubleshooting](troubleshooting.md).
