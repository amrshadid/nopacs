# The command line and the interactive program

[Documentation index](../README.md) · [Running NoPACs](../operate/running.md) · [Settings](../operate/settings.md)

Two programs, built and released together, with one version. `nopacs` is the
command line; `nopacs-server` is the server and the commands that touch the data
directory while the server is stopped.

`nopacs <noun> <verb> [name] [flags]`. `nopacs --help` lists the commands,
`nopacs <noun> <verb> --help` gives each one's flags with examples, and
`nopacs completion bash` (also `zsh`, `fish`, `powershell`) writes the shell
completion. The commands are clients of the API and can do nothing it does not
([internal/cli](../../internal/cli/doc.go)); the program links no database or
storage driver, so it can run on a machine that holds none of the server's data.

`nopacs-server <command>` (`nopacs-server --help` lists them) holds what must
run on the server's machine, some with the server stopped
([internal/servecmd](../../internal/servecmd/doc.go)): `init`, `serve`, `backup`
and `tenant import`, and its own `version`. Typed to `nopacs`, one of these
(`backup verify` and `backup restore` among them) exits 2 with one line naming
the `nopacs-server` command to run instead; `nopacs backup create` is the
backup over the API, while the server runs. Both
programs take the data directory as `--data-dir` or `NOPACS_DATA_DIR`.

## The commands

This table says what each command is for. Every command and every flag, with its type and default, is in [the command reference](../reference/commands.md), which is made from the programs themselves.

| Command | Verbs and what they do |
|---|---|
| `nopacs-server init`, `nopacs-server serve` | Make an installation; run its server. See [Running NoPACs](../operate/running.md) |
| `nopacs context` | `add <name> --server URL --key-file PATH [--ca-file PATH] [--tls-client-cert PATH --tls-client-key PATH] [--tenant NAME] [--replace]`, `list`, `show <name>`, `use <name>`, `remove <name>`. See [Contexts](#contexts) |
| `nopacs version`, `nopacs whoami` | The build; who the server takes the caller to be and what it may do |
| `nopacs tenant` | `create <name> [--db-env VAR \| --db-file PATH]`, `list`, `show`, `status <name>` (its receivers, storages, jobs in progress and senders with their health), `remove <name> --yes` |
| `nopacs key` | `create --holder TEXT [--expires 90d \| --no-expiry] [--network CIDR]... [--client-cert FILE \| --client-cert-sha256 HEX]`, `list`, `show`, `rotate <id> [--overlap 24h]`, `set <id>` (networks, client certificate), `revoke <id> [--with-successors]`, `reinstate (--all \| <id>...)` (local operator only: lets keys a restore suspended work again; see [Backup](../operate/backup.md#keys-come-back-suspended)) |
| `nopacs grant` | `set <key-id> --role reader\|writer\|operator\|admin (--tenant NAME \| --installation)`, `list (--key-id ID \| --tenant NAME \| --installation)`, `remove` |
| `nopacs setting` | `get`, `set <name> <value>`, `unset`, `list`, each with `[--tenant NAME]`. The names are in [Settings](../operate/settings.md) |
| `nopacs audit` | `list` (filters `--tenant`, `--key-id`, `--outcome done\|refused\|failed`, `--since`, `--until`); `verify [--tenant NAME] [--from SEQ] [--to SEQ] [--expect-seq N] [--expect-digest HEX]` (recomputes the hash chain and checks each signed checkpoint, calling again until the range is done; exits 1 with each problem listed when the log does not verify); `checkpoints [--tenant NAME]` (newest first, with each digest); `export [--tenant NAME] [--from SEQ] [--to SEQ] [--out FILE]` (JSON lines that `scripts/prove/audit_verify.py` checks with hashlib and openssl; with `--out` written to a new temporary beside `FILE`, mode 0600, and given the name `FILE` when whole, never replacing a file, one that appeared meanwhile included (exit 5), then a summary with the last digest to keep); `public-key [--version N [--installation ID]]` (the PEM of the key that checks the signatures, to keep outside the installation; `--json` lists every key; `--installation` names another installation's key, but this installation holds only its own keys, so another id is not found). See [Running NoPACs](../operate/running.md#check-that-the-audit-log-was-not-changed) |
| `nopacs storage` | `add <tenant> <name> --type dir\|s3 ...`, `list`, `show`, `set`, `remove --yes`, `test` |
| `nopacs receiver` | `add <tenant> <name> [--type dimse\|stow\|import] ...` (an import receiver: `--root ROOT --layout plain\|pattern [--pattern RE --unit-depth N] [--map-file PATH --map-key-column C... --map-key-part P... --map-value-column C --map-gives PART]`, see [Importing a directory](importing.md)), `list`, `show`, `set`, `remove --yes`; `--forward-to SENDER` (repeat for more) sends each reception on when it is archived; `--profile NAME@VERSION` de-identifies by that version, `--as-received` archives as received; `route list\|add\|set\|remove\|move\|default <tenant> <receiver> ...` chooses each study's storage, profile and senders by AE title and header values; `route export <tenant> <receiver> [--to FILE]` writes the routing with the receiver's update time, `route import <tenant> <receiver> FILE\|- [--ignore-update-time]` writes it back whole, refused (exit 5) when the receiver changed since the export ([Running NoPACs](../operate/running.md)) |
| `nopacs profile` | `create <tenant> <name> (--document\|--from) FILE\|- [--expect-latest N]`, `get <tenant> <name>[@<version>]`, `list`, `versions <tenant> <name>`, `export <tenant> <name>[@<version>] [--to FILE]`, `validate <tenant> --document FILE\|-`, `describe <tenant> (<name>[@<version>] \| --document FILE\|-) [--no-rules]` (what each class of element gets, in plain words, the method record a Conformance Statement needs: the PS3.16 CID 7050 codes and the (0012,0063) text; then every rule), `try <tenant> (--profile NAME[@VERSION] \| --document FILE\|-) [FILE\|DIR ...] [--record ID ...] [--changed-only] [--fail-on-held]`, `try --local --document FILE\|- FILE\|DIR ...` (the same trial in the command itself, with no server: a document or a version's export, keys made for the run, nothing sent), `explain <tenant> (--profile ... \| --document ...) (FILE \| --record ID)`, `diff <tenant> <name>[@<version>] <name>[@<version>]`, `lookup import <tenant> <table> FILE` (a CSV of value,code; all or nothing, conflicts answered by their count only; 10 an hour a key; admin), `lookup list <tenant>` (each table's count, no value). A document with a problem exits 2 with each problem listed; `try --fail-on-held` exits 5 when a file would be held. See [Running NoPACs](../operate/running.md) |
| `nopacs sender` | `add <tenant> <name> --type dimse --host H --port N --called-aet T --calling-aet T`, or `--type stow --url URL --authentication none\|basic\|bearer\|header` with the credential from `--credential-file`, `--credential-stdin` or `--credential-env`; `list`, `show`, `set`, `remove --yes [--cancel-jobs]`, `test` (exits 1 when the peer is not reached). See [Sending data on](sending.md) |
| `nopacs job` | `create <tenant> --sender NAME <subject>[/<session>[/<scan>]] [--wait]` (`--wait` follows the job as `watch` does, with its exit codes), `show` (also `get`), `list` (filters `--state`, `--in-progress`, `--sender`, `--since`, `--until`), `watch` (follows the event feed until the job ends, across the streams the server ends; exits 0 only for done), `cancel`, `retry`, `failures [--warnings]` (a DICOM status in hexadecimal, an HTTP status in decimal after `HTTP`); `import <tenant> <receiver> [<path>] [--map-file PATH \| --map-csv FILE] [--map-key-column C... --map-key-part P... --map-value-column C --map-gives PART] [--wait]` makes an import job, `files <tenant> <id> [--kind KIND]... [--unit ID]` lists its files that did not reach the archive by path and why, `units <tenant> <id> [--state STATE]...` its units with their counts (both operator only, since they answer paths). See [Importing a directory](importing.md) |
| `nopacs deid` | `check <tenant> [<subject>[/<session>[/<scan>]]] [--wait]` (verify archived instances against the profile version each records), `dry-run <tenant> (<profile>[@<version>] \| --own) [<target>] [--wait]` (what a version would archive and hold, and why); these two make a job of the kind deid that changes nothing, which `job show` (with its counts per rule and reason), `watch`, `failures`, `cancel` and `retry` work on. `code <tenant> (--patient-id VALUE\|- \| --attribute NAME --value VALUE\|-)` is the forward lookup, for a withdrawal of consent: the keyed hash of the value, the subject label each receiver gives it and whether the catalog holds it, and each lookup table's code; admin only, 60 a minute a key and 60 a minute a tenant (exit 7 past either), audited before the answer without the value; `-` reads the value from standard input |
| `nopacs watch` | `<tenant> [--type TYPE]... [--cursor CURSOR] [--count N]`: the tenant's events as they happen, following on when the server ends a stream, until interrupted or after N events |
| `nopacs reception` | `list` (filters `--receiver`, `--step`, `--in-progress`, `--since`, `--until`, `--study-uid`), `show`, `retry`, `discard --reason TEXT` |
| `nopacs quarantine` | `list` (filters `--state`, `--reason`, `--receiver`, `--reception`, `--sop-uid`, `--study-uid`, `--since`, `--until`), `show [--header]`, `release [--storage NAME] [--profile NAME@VERSION]`, `reject --reason TEXT` |
| `nopacs correction` | `delete <tenant> <subject>[/<session>[/<scan>]] [--instance ID] --reason TEXT [--preview] [--yes]` (shows what the delete would remove first, then asks for the target to be typed again unless `--yes`; `--preview` stops after it), `relabel <tenant> <subject>[/<session>[/<scan>]] --to LABEL --reason TEXT [--wait]`, `replace <tenant> <quarantined-file-id> --reason TEXT`, `list <tenant> [--kind KIND] [--state STATE]`, `show <tenant> <id>` (with what each storage did with each purged object, and the values a delete holds), `undo <tenant> <id> --reason TEXT`, `purge <tenant> <id> [--wait] [--yes]`, `allow <tenant> <id> (--subject LABEL \| --study UID \| --series UID \| --instance UID) --reason TEXT`. See [Correcting archived data](correcting.md) |
| `nopacs backup` | `create --out FILE` (admin on the installation): a backup of the running installation over the API, encrypted by the server, written beside `FILE` and named when whole and its SHA-256 is the summary's, never replacing a file (exit 5); one backup or export at a time (exit 7 for another); `--json` prints the summary |
| `nopacs-server backup` | `create --out FILE [--master-env VAR]`, `verify FILE [--key-file PATH]`, `restore FILE [--key-file PATH] [--master-env VAR] [--scratch-schema NAME] [--take-over]`: commands of `nopacs-server` that take `--data-dir`; create and restore need the server stopped. `nopacs tenant export <name> --out FILE --key-out FILE` (admin, `nopacs`), `nopacs-server tenant import FILE --transfer-key FILE [--db-env VAR] [--master-env VAR] [--scratch-schema NAME] [--take-over]` (on the server's machine, server stopped) and `nopacs tenant reclaim <name>` (local operator only, `nopacs`) move a tenant. See [Backup, restore and moving a tenant](../operate/backup.md) |
| `nopacs catalog` | `subjects <tenant>`, `sessions <tenant> [<subject>]`, `scans <tenant> [<subject> [<session>]]`, with filters and `--order` |
| `nopacs scan show` | `<tenant> <subject>/<session>/<scan> [--metadata]`: a scan, and the DICOM header of every instance as JSON |
| `nopacs download` | `<tenant> <subject>[/<session>[/<scan>]] --to DIR [--quiet]` |

A command that removes or discards something says what it would do and exits
2 until it is given `--yes`.

### Scrub and rebuild

```
nopacs job scrub <tenant> [--storage NAME] [--wait]
nopacs job rebuild <tenant> --mode compare|replace [--from-storage NAME] [--scratch-schema NAME] [--yes] [--wait]
nopacs job findings <tenant> [--kind missing|orphan|damaged|disagreeing|manifest-damaged]... [--storage NAME] [--cleared] [--limit N | --all]
nopacs job finding <tenant> <id>
```

- **`job scrub`** reads every stored object again and compares its length and
  SHA-256 with the catalog and the manifests. It reports and marks, and never
  changes a stored byte. Operator role. Exit 5 while another scrub of that storage
  is queued or running.
- **`job rebuild`** builds a shadow catalog from the storages. `compare` tallies the
  differences from the catalog in use and drops the shadow (`--from-storage` builds
  it from one storage alone). `replace` (admin role, needs `--yes`; on a terminal the
  tenant's name is typed again) makes the shadow the catalog in use. Until the job
  ends, receivers refuse new data, corrections and purges are refused, and scans the
  build leaves out are removed from the catalog in use (their files stay on the
  storage). Run a compare first. `--scratch-schema` is for Postgres only.
- **`job findings`** and **`job finding`** list and show what scrubs found (reader
  role). Held findings are listed by default; `--cleared` adds those a later scrub
  read good. The table never shows an object's key; `--json` does. A replace rebuild
  does not rewrite stored objects, so a finding holds after it until the object
  reads good again.

`job watch` and `--wait` follow these jobs like any job, and exit 1 when a scrub
ends partial (it found something). `job list` shows the columns ID, KIND, ON, STATE,
DONE, OK, NOT OK, REMAINING and CREATED.

## How a command behaves

- **Where it connects.** By default the local socket `run/nopacs.sock` in the
  data directory (`--data-dir` or `NOPACS_DATA_DIR`), as the user that runs the
  server, with no key. With `--server https://host:port` (or `NOPACS_SERVER`) it
  connects over TLS with the key from `NOPACS_KEY` or the file named by
  `--key-file`. `http://` is accepted only for loopback hosts. A call is
  bounded by `--timeout` (30 seconds).
- **Contexts.** `--context NAME` (or `NOPACS_CONTEXT`, or the current context)
  takes the server, the key file and the TLS files from a saved context; see
  [Contexts](#contexts).
- **A secret is never an argument.** No flag takes a key, a password or a
  database address: a tenant's Postgres address is read from the variable named
  by `--db-env` or the file named by `--db-file`, an S3 storage's secret access
  key from `--credential-env` or `--credential-file`, a server's master database
  address from `--master-env`. Filters that take a key's public id are
  `--key-id`; a whole key given where an id goes is refused. `--key-file` must
  name a file only you can open (`chmod 600`).
- **TLS.** TLS 1.2 or later. `--ca-file` (or `NOPACS_CA_FILE`) names the
  certificate authorities to trust for `--server` instead of the system's. A key
  that works only with a TLS client certificate is used with `--tls-client-cert`
  and `--tls-client-key` (or `NOPACS_TLS_CLIENT_CERT` and `NOPACS_TLS_CLIENT_KEY`).
- **Output.** A table, or with `--json` one document that starts with
  `"schemaVersion": 1` ([internal/cli/output](../../internal/cli/output/doc.go)).
  Times are shown in the local zone with their offset; there is no color when
  `NO_COLOR` is set or the output is not a terminal.
- **Errors** are one line on standard error with the server's message and its
  request id. Exit codes: 0 done, 1 failed, 2 wrong usage, 3 refused
  (unauthenticated or denied), 4 not found, 5 a state that forbids the change (a
  storage that holds files, a port not allotted), 6 a position that can no longer
  be answered (an event cursor too old), 7 a bound of the server reached.
- **Lists** fetch up to `--limit` (100) items and say when more exist; `--all`
  fetches every item.
- **The catalog** serves only the combinations of filter and order an index
  answers, and the server names them when it refuses one: newest first by
  default; a label prefix with `--order label`, a study date range with
  `--order study-date`, a description prefix with `--order series-description`.
  `catalog scans` of a subject named alone lists by label.
- **Downloads** are laid out `<dir>/<subject>/<session>/<scan>/<instance>.dcm`,
  each written as `.part` and renamed only once its SHA-256 matches the one the
  server announced. Run again, a download skips what is complete and resumes a
  `.part` by range ([internal/cli/download](../../internal/cli/download/doc.go)).

## Contexts

A context names one way to reach one installation: the server's address, the
file that holds a key, the certificate authorities and client certificate files
for it, and optionally a default tenant. Contexts are kept in
`$XDG_CONFIG_HOME/nopacs/contexts.json` (`~/.config/nopacs/contexts.json` when that
is unset, on macOS too; `%AppData%\nopacs\contexts.json` on Windows), or in the
file named by `NOPACS_CONFIG`. The file holds paths, never a key or any other
secret, and a file with a field this program does not know is refused.

- **The file is private.** On Unix it is read only when its mode is 0600 and it
  is yours, as `--key-file` is, and when its directory is yours, not writable by
  others and without a macOS access list entry; the default directory, which
  `nopacs` makes 0700, must also not be readable by others. A `NOPACS_CONFIG` file
  must be in a private directory of your own, not a shared one such as `/tmp`. On
  Windows the file's owner and access list are checked (the directory's are not).
  It is written whole to a private temporary file in the same directory, flushed
  and renamed over the old one, and a save is refused (exit 5) when the file
  changed since the command read it.
- **A hand-edited file cannot leak.** Loading refuses a server with a user name or
  password, a path that is not absolute, a value that is a key (starting
  `nopacs_k`), an invalid tenant name and a field it does not know, and no message
  quotes a value.
- **`add`** takes the flags every command takes (`--server`, `--key-file`,
  `--ca-file`, `--tls-client-cert`, `--tls-client-key`) and `--tenant` for the
  default tenant. Only those flags are used, not the environment, so what is
  checked with a `WhoAmI` call is what is saved. A name that exists is refused
  (exit 5) unless `--replace` is given; the first context saved becomes the
  current one. Paths are saved as absolute paths.
- **Which context.** `--context NAME`, else `NOPACS_CONTEXT`, else the current
  context (`context use`). A name that is not in the file is exit 4, naming the
  context and the file. A context and a server named another way do not mix:
  `--context` or `NOPACS_CONTEXT` together with `--server`, `--data-dir`,
  `NOPACS_SERVER` or `NOPACS_DATA_DIR` is a usage error (exit 2) naming both. The
  current context applies only when none of those is given; with no context and
  none of those, a command is a usage error as before.
- **The key.** A context's key file is sent only to the context's own server.
  `--key-file` overrides it. When `NOPACS_KEY` is set, a command that uses a context
  is refused (exit 2), so an exported key never travels to whichever context is
  current. `--ca-file`, `--tls-client-cert` and `--tls-client-key` and their
  variables override the context's files.
- **The default tenant** fills `--tenant` only where it is optional: `setting get`,
  `set`, `unset` and `list`, and `audit list`, `verify`, `checkpoints` and
  `export`. Without `--tenant` those act on the
  installation, so with a default tenant they act on the tenant instead, which
  the command says on standard error ("using tenant lab1 from context prod");
  `--tenant ""` says the installation. A command that takes the tenant as an
  argument (`storage list <tenant>`) is unchanged.

```sh
nopacs context add lab1 --server https://archive.example.org:8443 \
    --key-file ~/.nopacs/lab1.key --ca-file ~/.nopacs/ca.pem --tenant lab1
nopacs context list
nopacs --context lab1 tenant list
```

## The interactive program

`nopacs` with no arguments, when standard input and output are both a terminal,
opens a program with a menu, lists that scroll and detail screens: tenants (with
each tenant's grants, settings and audit log, its status that follows its events,
and its storages, receivers, senders with their health and a test of the peer,
jobs with a progress bar that follows the events, a job's failures, with cancel
and retry, receptions and quarantine, where a held file is released or
rejected, replicas with their repair and switch, retention rules with the sessions
due and a disposal, holds, corrections with their previews, scrub findings, profiles
and a receiver's routes, and the audit seal's verification), the subjects,
sessions and scans of a tenant with a scan's headers and a download of what is
shown, keys, settings and the audit log. The menu and the keys of each screen
are built from the caller's grants (a reader sees no Keys or Settings, and a
tenant's screens offer only what its role on the tenant allows); the server
checks every call whatever the menu offers. It
finds the server with the same flags and environment as the commands and is a
client of the same API ([internal/tui](../../internal/tui/doc.go)). With any
argument, or with either end not a terminal, `nopacs` prints the usage line and
exits 2.

- **Keys.** On a tenant, `n` opens its senders and `w` its jobs; on a job, `f`
  its failures, `c` cancels it and `r` retries it. Arrows or `j`/`k`, page up and down, enter to open, esc to go back
  (or to cancel a call that is waiting), `/` to filter the loaded rows, `?` for
  the keys of the screen, `q` to quit. A list loads 100 rows and the next 100
  when the cursor nears the end.
- **A new key** (`n` on the key list) is shown once, alone, and leaving that
  screen asks once more.
- **Nothing destructive happens on one key.** Revoking a key and removing a
  grant, a setting override or a tenant need its id or name typed; so do
  deleting, relabeling, replacing, undoing and purging data (a delete shows its
  preview first), detaching a replica, repairing a primary, removing a rule,
  releasing a hold and disposing of sessions. A switch is confirmed by typing the
  first characters of the token its preview showed, or the primary's name.
- **Terminal.** It fits 80 by 24 and redraws on resize; there is no color when
  `NO_COLOR` is set, with `TERM=dumb`, or on Windows. The terminal is restored
  on quit, on SIGTERM and SIGINT, and on a panic.
