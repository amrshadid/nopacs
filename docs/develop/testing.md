# Testing

[Documentation index](../README.md) · [Gates and outside checks](gates.md) · [Architecture](architecture.md)

How the tests and checks are run, what `-short` means here, how a slow test is
marked, and the `NOPACS_*` environment variables the tests and scripts read. Every variable
the repository reads, the programs' as well, is listed in [the generated reference](../reference/environment.md).

## Running the tests

| Command | What it runs |
|---|---|
| `make gates` | The gates before each push, and never a scale test: `check-comments`, `fmt`, `go vet` on 64 bits, linux/386 and windows, `lint`, and `make test`. |
| `make test` | `go test -race -count=1 ./...`: every package under the race detector. A test that needs a service (Postgres, S3, a tool such as dcmtk or sqlite3) skips, saying why, when the service is not there. |
| `make test-scale` | The tests that measure at scale, without the race detector (see below). Takes minutes. |
| `make test-postgres` | The database drivers' tests against a Postgres, failing if there is none (`NOPACS_TEST_POSTGRES`, or the container `make pg-up` starts). |
| `make test-s3` | The S3 driver's tests against each S3 server `make s3-up` starts, failing if one is not up. |
| `make test-386` | The tests as 32-bit binaries, on a Linux x86-64 host. |
| `make fuzz` | Each fuzz target for `FUZZTIME` (10 seconds unless given). |
| `go test -short ./...` | Only the tests that start no process and take under a second. |

The outside checks use a tool that is not NoPACs' own code, so a result is
not only the program agreeing with itself. [Gates and outside checks](gates.md)
says how each is run and what it needs:

| Command | What it checks against what |
|---|---|
| `make check-seal` | The sealed-secret format, against Python's cryptography package |
| `make check-metadata` | The Metadata service's answers, against pydicom's `Dataset.to_json_dict()` |
| `make check-catalog` | The Catalog service's answers, against what pydicom reads of the received files |
| `make check-frames` | Each file's frame table, against the fragments pydicom divides each frame into |
| `make check-dicomweb` | QIDO-RS and WADO-RS, from dicomweb-client, against pydicom's reading of the files received |
| `make check-stow` | STOW-RS in a real server process, from dicomweb-client, against the bytes sent and pydicom |
| `make check-master-sqlite`, `make check-tenant-sqlite` | The SQLite databases, read back with the `sqlite3` tool |
| `make check-master-postgres`, `make check-tenant-postgres` | The Postgres databases, read back with `psql` and `pg_dump` |
| `make check-server` | The server's contract, with grpcurl, Python's gRPC client and curl |
| `make check-e2e` | The real program from `init` to a restart, each command a process |
| `make check-json` | Every command's `--json` output, parsed by Python's `json` module |
| `make check-completion` | The shell completion script |
| `make check-tui` | The interactive program on a real pseudo-terminal, driven from Python |
| `make prove` | The proof script: checks made from outside the binary |
| `make outside` | Every outside check that needs a tool that is not Go |
| `make ci` | Every job of the CI workflow on one machine |

## The scripts

The outside checks and the proof are Python and shell under `scripts/`. The
Python is one library of plain functions with a thin file in front of each
use, so a helper is written once:

```
scripts/
  lib/
    nopacs_scripts/     the Python library; functions only
      report.py entry.py  a check that cannot go on raises Failure; the entry prints it and sets the exit status
      paths.py keys.py net.py proc.py db.py digest.py dicom.py proof.py
                          the repository's paths, the key file and HKDF, free ports, building the two
                          binaries, a catalog read by SQLite, SHA-256, pydicom readings, the result
                          lines and server handling every proof shares
      formats/            the audit chain and backup file formats, written from their descriptions
      deid.py read.py send.py importing.py whole.py interrupt.py
                          what each proof builds its studies, trees and installation from
      checks/             the body of each file under scripts/check
      tools/              the auditor's tools (audit_verify, backup_open, catalog_dump) and their self-tests
      sections/           the body of each proof under scripts/prove, and the steps it is made of
    sh/                 check.sh, psql.sh, sqlite.sh: the shell the database checks share
  check/                one entry per outside check (Python, or shell for a database)
  prove/                run.sh (the order of the proof of this version, the shell sections beside it),
                        one entry per Python proof, viewer_driver.py (which runs inside a browser
                        container and so stands alone)
  requirements/ tools/  pinned requirements; the checked installers of buf and grpcurl
```

An entry is a few lines: it puts `scripts/lib` on `sys.path` (so it works
also under `python -I`, which leaves the script's own folder off it), calls the
module's `main(argv)` and exits with what it returns. A function in the library
takes what it needs as arguments and returns data; it reads no command line and
does not end the process. A failure that must stop a run is a `Failure` the
entry prints. What several scripts share for processes, sockets and databases
is in `proc`, `net` and `db`; a check or a proof step still runs its own tools
where only it needs them. The make
targets are the way to run a script; their names and arguments are the same
as the entries' paths.

## What `-short` means

`-short` means "start no process and take under a second". A test that starts a
child process, a server or a tool, or that takes longer, calls `t.Skip` under
`testing.Short()`. `go test -short ./...` is therefore a quick check that needs
nothing installed. The ordinary run (`make test`) does not use it.

## How a slow test is marked

The build tag `interrupt` adds a kill at each named durable step of a
correction and a rebuild to the server (`internal/interrupt`); without it,
`interrupt.At` is empty and the compiler removes it. Only `make
prove-interrupt` builds with it, and the lint target runs once more with the
tag.

The build tag `clock` makes the server's time movable (`internal/clock`): without it `clock.Now` is `time.Now`; with it, the offset in `NOPACS_CLOCK_OFFSET` or `NOPACS_CLOCK_FILE` is added to points in time, while waits and deadlines stay on real time. `scripts/prove/s3_stub.py` is an archive tier for the proofs (a proxy in front of a real S3 server that answers a GET of an archived key with 403 InvalidObjectState, shows `x-amz-storage-class` and `x-amz-restore`, and starts and completes restores, with a control port); `scripts/prove/tree_compare.py` compares two trees (directories or `s3://bucket/prefix`) by key, size and SHA-256; `scripts/prove/clock_offset.py` moves the time of a server built with the tag. Each has `--self-test`, run by `make check-prove-tools`.

`make prove-whole` is an outside proof, not a Go test. Two of its groups
(`versions`, `room`) start an S3 server (Silo, at the digest `S3_SILO_IMAGE`
pins) in a container named for the run and remove it when the group ends; they
need docker reachable through `DOCKER_HOST` and boto3
(`scripts/requirements/prove-whole.txt`). Every other group needs no container.

A test that measures at scale is named `TestScale*` and runs only in its own
run, `make test-scale`; the ordinary run skips it. A test that needs a service
asks for it through an environment variable below and skips when the variable
is unset (or fails, when the matching `NOPACS_REQUIRE_*` variable is set, as
`make test-postgres` and `make test-s3` set it).

The catalog is measured at scale by `TestScaleCatalog` in each database
driver, on a synthetic tenant `testkit.GenerateTenant` writes through the
tenant port: it runs only when `NOPACS_SCALE_INSTANCES` says how large, since
ten million instances take from minutes to hours. `TestScaleCatalog` is
skipped in `make test-scale` without it. One run of each:

```
NOPACS_SCALE_INSTANCES=10000000 NOPACS_SCALE_SUMMARY=$GITHUB_STEP_SUMMARY \
  go test -count=1 -v -timeout 6h -run '^TestScaleCatalog$' ./internal/driver/sqlite
NOPACS_TEST_POSTGRES=postgres://... NOPACS_SCALE_INSTANCES=10000000 NOPACS_SCALE_SUMMARY=$GITHUB_STEP_SUMMARY \
  go test -count=1 -v -timeout 6h -run '^TestScaleCatalog$' ./internal/driver/postgres
go test -run '^$' -bench BenchmarkPackedScan -benchtime 10x ./internal/blob/dir
NOPACS_TEST_S3=http://KEY:SECRET@host:port/bucket go test -run '^$' -bench BenchmarkPackedScan -benchtime 10x ./internal/blob/s3
```

A Postgres tenant takes about 1.7 KB of disk per instance, tables and
indexes together. The storage drivers' benchmark
`BenchmarkPackedScan` reads a scan of 500 small instances kept one object per
instance and kept as one packed object read by ranges.

The SQLite driver's event feed is measured at scale by `TestScaleEventFeed` (a
million events) and `TestScaleWriterBesideIdleWatches` (a thousand idle
watches). Both run in `make test-scale` and are skipped under `-short`, under
the race detector, on 32 bits and on Windows. They take no variable.

## Environment variables

Most variables below are read by tests and scripts. The four that the program
itself reads are listed first.

### Read by the program

| Variable | What it does | Default | Read by |
|---|---|---|---|
| `NOPACS_DATA_DIR` | The data directory: where `init` and `serve` work, and whose `run/nopacs.sock` the commands reach. `--data-dir` wins. | none: a command that needs one says so | `init`, `serve`, every command; the end-to-end and interactive-program tests and checks, the proof script |
| `NOPACS_SERVER` | The address of a server on the network, reached over TLS. `--server` wins. | none | every command; the command-line tests, the end-to-end test, the proof script |
| `NOPACS_KEY` | The key used over the network; the only place a key comes from besides `--key-file`. | none | every command; the same tests and scripts |
| `NOPACS_CA_FILE` | A PEM file of certificate authorities to trust for the network, in place of the system's. `--ca-file` wins. | the system's | every command; the command-line tests, the end-to-end test, the proof script |
| `NOPACS_TLS_CLIENT_CERT` | A PEM file of a TLS client certificate to present to the server on the network, for a key that works only with it. `--tls-client-cert` wins. | none | every command; the command-line tests |
| `NOPACS_TLS_CLIENT_KEY` | The PEM file of that certificate's private key, private to its owner. `--tls-client-key` wins. | none | every command; the command-line tests |

### Select or enable a test

| Variable | What it does | Default | Read by |
|---|---|---|---|
| `NOPACS_TEST_POSTGRES` | The address of a scratch Postgres database (`postgres://...`) the tests may make schemas in; each test that makes a tenant there gets a schema of its own (`testkit.PostgresSchema`). | unset: the Postgres tests skip | `internal/driver/postgres`, `internal/receive`, `internal/servecmd`; `make test-postgres`, `make outside-postgres`; `scripts/check/master-postgres.sh`, `scripts/check/tenant-postgres.sh` (required) |
| `NOPACS_REQUIRE_POSTGRES` | When set to anything, a missing Postgres address is a failure instead of a skip. | unset | `internal/driver/postgres`, `internal/receive`, `internal/servecmd`; set by `make test-postgres` and by CI |
| `NOPACS_TEST_S3` | How to reach a scratch S3 server, as `http://KEY:SECRET@host:port/bucket`, with `region=NAME` and `pathstyle=0` in the query when needed. | unset: the tests against a server skip | `internal/blob/s3`, `internal/receive`, `internal/servecmd`; `make test-s3` sets it for each server; the bucket must exist (the S3 driver's tests make it) |
| `NOPACS_REQUIRE_S3` | When set to anything, a missing S3 server is a failure instead of a skip. | unset | `internal/blob/s3`, `internal/receive`, `internal/servecmd`; set by `make test-s3` and by CI |
| `NOPACS_TEST_SCALE` | When set, runs the scale tests (`TestScale*`, and the tests of the SQLite and Postgres drivers and the registry that work at millions of rows or 1,000 tenants); without it they skip, so a plain `go test` stays quick. | unset: they skip | `internal/receive`, `internal/driver/sqlite`, `internal/driver/postgres`, `internal/registry`; set by `make test-scale` |
| `NOPACS_TEST_ALL_SEEDS` | When set, `TestPipelineModelAllSeeds` runs all 150 seeds of the SQLite pipeline model (minutes, without the race detector); the everyday run takes 8 seeds (6 under the race detector). | unset: it skips | `internal/driver/sqlite`; set by `make test-scale`, which CI runs |
| `NOPACS_SCALE_INSTANCES` | How many instances the catalog's scale tests make, through the tenant port, from a seed (`testkit.GenerateTenant`); they then log reception's rate into the empty and the large tenant, table and index sizes, the plan and the median and 99th percentile time of each catalog read, and the time of `VACUUM`. Ten million takes hours on SQLite and about 1.7 KB of database per instance on Postgres. No make target sets it. | unset: they skip | `internal/driver/sqlite`, `internal/driver/postgres` (`TestScaleCatalog`) |
| `NOPACS_SCALE_SUMMARY` | A file the catalog's scale tests append their numbers to as a Markdown table (in CI, `$GITHUB_STEP_SUMMARY`); the table is logged either way. | unset: logged only | `internal/driver/sqlite`, `internal/driver/postgres` (`TestScaleCatalog`) |
| `NOPACS_SCALE_KEEP` | When set to anything, the catalog's scale tests keep the database they made (a SQLite file in the temporary directory, a Postgres schema `scale_...`) and log where it is, so its plans can be read again with `sqlite3` or `psql`; whoever set it removes it. | unset: removed at the end | `internal/driver/sqlite`, `internal/driver/postgres` (`TestScaleCatalog`) |
| `NOPACS_SCALE` | When set, the event feed's adversarial test runs its long form (400 operations instead of 60). No make target sets it. | unset | `internal/driver/sqlite` (`TestEventFeedUnderAdversaries`) |
| `NOPACS_MEASURE` | When set, runs the measurement of the gap between the two denial paths; it is a measurement, not a check. Run it without the race detector. | unset: it skips | `internal/access` (`TestMeasureDeniedGap`) |
| `NOPACS_BLOB_SMALL_VOLUME` | A directory on a small volume (a tmpfs of a few megabytes, say) for the full-volume test of the directory storage driver. | unset: it skips | `internal/blob/dir` (`TestFullVolumeKeepsOldObject`) |
| `NOPACS_E2E` | When set, runs the real program end to end (about fifty processes). | unset: it skips | `cmd/nopacs-server` (`TestRealServerEndToEnd`); set by `make check-e2e` |
| `NOPACS_TEST_OTHER_USER` | The name of a second operating system user, for the test that the local socket refuses another user. It needs `sudo -n -u` to work without a password. | unset: it skips | `internal/server`; set by CI; `make ci` runs the test when it is set |
| `NOPACS_SQLITE3_REQUIRED` | When set to anything, a missing `sqlite3` tool is a failure instead of a skip. | unset | `internal/driver/sqlite` (kill and plan tests); set by `scripts/check/master-sqlite.sh` |
| `NOPACS_PYTHON` | The Python, with pynetdicom and pydicom, that the DICOM receiver's outside tests use, and that the held-file header test compares against pydicom's `to_json_dict` with. | `python3` on the `PATH` for the receiver's tests; unset skips the pydicom comparison of the header test | `internal/receive/dimse`, `internal/servecmd` (`TestAHeldFilesHeaderIsAnsweredByTheRealServer`), `internal/send` (`TestOutsidePynetdicom`; `make check-send`), `internal/dirimport` (`TestOutsidePydicomReadsTheImport`, which needs pydicom only) |
| `NOPACS_DCMTK` | A directory holding dcmtk's programs, looked in before the `PATH` (another program of the same name may come first on it). | the `PATH` only; the test skips when dcmtk is not found | `internal/receive/dimse`, `internal/servecmd`, `internal/send` (`TestOutsideStorescp`); set by `make test-postgres`, `make test-s3` and `make check-send` from `DCMTK` |
| `NOPACS_REQUIRE_DCMTK` | When set to anything, a missing dcmtk `storescu` is a failure instead of a skip. | unset | `internal/servecmd`, `internal/send`; set by `make test-postgres`, `make test-s3` and `make check-send` |
| `NOPACS_RECEIVE_CRASH_ONLY` | A comma-separated list of crash points to try, instead of all of them. | unset: every point | `internal/receive` (crash test) |

### Written for a script or an outside check

| Variable | What it does | Default | Read by |
|---|---|---|---|
| `NOPACS_CHECK_JSON_DIR` | The directory the test writes each command's `--json` output into, for `scripts/check/cli-json.py`. | unset: the test skips | `internal/cli` (`TestWriteJSONOutputs`); set by `make check-json` |
| `NOPACS_CHECK_SEAL_PYTHON` | The Python, with the cryptography package, that the sealed-format check runs. Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the test skips | `internal/secret` (`TestSealOpensInPython`); set by `make check-seal` |
| `NOPACS_CHECK_REBUILD_PYTHON` | The Python (standard library only) that runs `scripts/prove/catalog_dump.py` on a tenant's catalog and a rebuild's shadow: the corpus archived through the real pipeline, a compare's shadow equal to the live catalog, a replace after every catalog row is removed equal to the dump before, a scan a correction record deletes absent from the shadow. Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the test skips | `internal/rebuild` (`TestRebuildChecksOutside`) |
| `NOPACS_CHECK_REBUILD_PSQL` | The command line of the `psql` that check reads a Postgres catalog with, split at spaces, such as `docker exec -i nopacs-test-postgres psql -U postgres`. | unset: the Postgres part skips | `internal/rebuild` (`TestRebuildChecksOutside`, Postgres part) |
| `NOPACS_CHECK_BACKUP_PYTHON` | The Python, with the cryptography package, that the backup-file check runs (`scripts/check/backup-cipher.py`: HKDF over `hmac` and `hashlib`, AES-GCM from cryptography). It opens a backup Go wrote, refuses it cut, reordered, changed and extended, and writes one Go opens. In `internal/backup` the same Python runs `scripts/prove/backup_open.py --sections` on a whole backup Go wrote (every section's SHA-256 against the footer, a changed byte refused) and `scripts/prove/catalog_dump.py` on the catalog before a backup and after its restore, which must be equal. Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the test skips | `internal/secret` (`TestBackupOpensInPython`), set by `make check-backup-cipher`; `internal/backup` (`TestBackupOpensOutside`, `TestRestoreRoundTrip`) |
| `NOPACS_CHECK_RECORD_MAC_PYTHON` | The Python that the correction-record MAC check runs (`scripts/check/record-mac.py`: HKDF over `hmac` and `hashlib`). Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the test skips | `internal/core/record` (`TestRecordMACInPython`), set by `make check-record-mac` |
| `NOPACS_CHECK_EDIT_PYTHON` | The Python, with pydicom, that runs `scripts/check/edit-pydicom.py` over the revisions an edit of header values wrote: each new revision read as its old one with only the edit's changes (every other element as pydicom decodes it, Pixel Data byte for byte, the transfer syntax; the character set the old one or `ISO_IR 192`), on the primary and the replica, and the old value again after an undo. Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the pydicom reading is skipped, the rest of each test runs | `internal/correct` (`TestEditMakesNewRevisionsOnEveryCopy`, `TestEditPurgeRemovesTheSupersededRevisionEverywhere`); set by `make check-edit` |
| `NOPACS_CHECK_BACKUP_PSQL` | The command line of the `psql` that `catalog_dump.py` reads a restored tenant's Postgres catalog with, split at spaces, such as `docker exec -i nopacs-test-postgres psql -U postgres`. | unset: the Postgres catalog is not compared | `internal/backup` (`TestRestoreRoundTrip`, Postgres part) |
| `NOPACS_CHECK_AUDITCHAIN_PYTHON` | The Python (standard library only) that recomputes the audit chain's entry encoding and heads with `hashlib` and has openssl verify each checkpoint's Ed25519 signature (`scripts/check/auditchain.py`). Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the test skips | `internal/core/auditchain` (`TestChainChecksInPython`), `internal/auditseal` (`TestSealChecksOutside`, on a sealed SQLite log and, with `NOPACS_TEST_POSTGRES`, a Postgres one); set by `make check-auditchain` |
| `NOPACS_CHECK_AUDITCHAIN_OPENSSL` | The openssl that verifies the checkpoints' signatures with `pkeyutl -verify -rawin`; it must be OpenSSL 3.0 or later (the macOS system LibreSSL does not take Ed25519 there). | unset: `openssl` on the `PATH` | `internal/core/auditchain` (`TestChainChecksInPython`), `internal/auditseal` (`TestSealChecksOutside`); set by `make check-auditchain` |
| `NOPACS_CHECK_AUDITCHAIN_SQLITE3` | The `sqlite3` shell that reads a sealed SQLite log's tables directly (each entry's seq, the checkpoints, the anchors) to compare with the export. | unset: `sqlite3` on the `PATH` | `internal/auditseal` (`TestSealChecksOutside`) |
| `NOPACS_CHECK_AUDITCHAIN_PSQL` | The command line of the `psql` that reads a sealed Postgres log's tables, split at spaces, such as `docker exec -i nopacs-test-postgres psql -U postgres -d nopacs_test` for a server in a container. | unset: `psql` on the `PATH` with the address of `NOPACS_TEST_POSTGRES` | `internal/auditseal` (`TestSealChecksOutside`, Postgres part) |
| `NOPACS_CHECK_AUDIT_EXPORT_PYTHON` | The Python (standard library only) that runs `scripts/prove/audit_verify.py` on what the real program exports: a server writes audit in the installation's log and a tenant's, stops (its last pass seals them) and starts again; `nopacs audit verify`, `audit export` and `audit public-key` run as processes; the tool must accept each export with that key alone and with the digest the export printed, and refuse a copy with one entry's method changed and one with that entry removed, naming its seq. Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the test skips | `cmd/nopacs-server` (`TestAuditExportChecksOutside`); set by `make check-audit-export` |
| `NOPACS_CHECK_AUDIT_EXPORT_OPENSSL` | The openssl the tool verifies each checkpoint's signature with; 3.0 or later (the macOS system LibreSSL does not take Ed25519). | unset: `openssl` on the `PATH` | `cmd/nopacs-server` (`TestAuditExportChecksOutside`) |
| `NOPACS_CHECK_METADATA_PYTHON` | The Python, with pydicom, that compares the Metadata service's answers with `Dataset.to_json_dict()` (`scripts/check/metadata-pydicom.py`). Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the test skips | `internal/api` (`TestMetadataEqualsPydicom`); set by `make check-metadata` |
| `NOPACS_CHECK_CATALOG_PYTHON` | The Python, with pydicom, that compares the Catalog service's answers (subjects, sessions, scans, their counts and bytes, and the study date, series description and modality lists) with what pydicom reads of the received files (`scripts/check/catalog-pydicom.py`). Set, even to nothing, it means the check was asked for and a missing interpreter fails. `NOPACS_PYDICOM_CORPUS` adds files to the archive. | unset: the test skips | `internal/api` (`TestCatalogEqualsPydicom`); set by `make check-catalog` |
| `NOPACS_CHECK_DOWNLOAD_PYTHON` | The Python that checks the plain HTTP download of instances, from the real server's local socket after a reception through it, with curl (whole, resumed in two ranges and by `curl -C -`, a range past the end, If-Range with another tag, HEAD) and hashlib against each scan's manifest as the storage holds it (`scripts/check/download-curl.py`); curl must be on the `PATH`. Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the check is left out of the test | `internal/servecmd` (`TestAStudyIsArchivedThroughServe`, which needs dcmtk's `storescu`) |
| `NOPACS_CHECK_FRAMES_PYTHON` | The Python, with pydicom, that compares each file's frame table (where each frame lies in the file) with the fragments pydicom divides each frame into, the item offsets walked by the script itself, and the bytes each range names with pydicom's (`scripts/check/frames-pydicom.py`). It runs with `-I`, so the pydicom it uses is the one installed; with pydicom 3 the division is `encaps.generate_fragmented_frames` (Extended Offset Table included), with pydicom 2 `encaps.generate_pixel_data`. Set, even to nothing, it means the check was asked for and a missing interpreter fails. `NOPACS_PYDICOM_CORPUS` adds every corpus file with pixel data and two files the script builds from it. | unset: the test skips | `internal/receive/frames` (`TestTableEqualsPydicom`); set by `make check-frames` |
| `NOPACS_CHECK_DEID_PYTHON` | The Python, with pydicom, that reads every file the de-identification engine's corpus test produces (`scripts/check/deid-pydicom.py`): for each pair of a public file and its de-identified result, it gathers from the original, with pydicom, the values of the attributes the test profile removes or replaces (names, identifiers, dates, UIDs, at any depth; values under 4 characters and registered UIDs skipped), and searches every element of the result for them, file meta, private elements and items included, Pixel Data excepted; it fails on any value found, on a result without Patient Identity Removed YES, and when no value was searched for. Set, even to nothing, it means the check was asked for and a missing interpreter fails. `NOPACS_PYDICOM_CORPUS` adds pydicom's files to the receiver's. A file of a pair pydicom cannot read is reported as refused and fails. | unset: the outside check is left out of the test | `internal/deid/engine` (`TestCorpusFilesAreDeidentified`; `TestValuePatternsAgreeWithPydicom`: an MRN and an accession-like number planted into each public file, de-identified by a profile with a value pattern that redacts and one that removes, and `scripts/check/deid-valuepattern.py` confirming with pydicom each redacted value and each removal and finding no matched text anywhere in the result, beside `deid-pydicom.py`; a control keeping a copy in a coded string is found by both), `internal/deid/check` (`TestAgreesWithPydicom`), `internal/receive` (`TestCorpusThroughADeidentifyingReceiver`, with `NOPACS_PYDICOM_CORPUS`: every corpus file sent through a receiver that de-identifies, and each archived file checked beside its original) |
| `NOPACS_CHECK_J2K_OPENJPEG` | The directory holding OpenJPEG's `opj_compress`, `opj_decompress` and `opj_dump` (2.5 or later, for HTJ2K), the outside check of the JPEG 2000 walk: over the codestreams of `internal/deid/engine/testdata/j2k`, the frames of the JPEG 2000 and HTJ2K files (with `NOPACS_PYDICOM_CORPUS`, pydicom's too) and eight codestreams `opj_compress` writes with a name in a comment (tiles and tile-parts with PLT and TLM, SOP and EPH, precincts, a progression change, an upshifted component, irreversible and lossy, no decomposition), a comment holding a name is planted in the main header and in every tile-part header; OpenJPEG must decode the planted codestream as the one received (or the check is not sound), and decode what the walk leaves to the very same samples, a PGX file a component, with `opj_dump` reading its header. With `NOPACS_CHECK_DEID_PYTHON` also set, the eight images are wrapped as DICOM files and de-identified, and `scripts/check/deid-j2k-pydicom.py` has pydicom (with pylibjpeg-openjpeg or Pillow) decode each result to the samples of its input and find no name in it; a pydicom without a JPEG 2000 decoder fails. | unset: the test skips | `internal/deid/engine` (`TestJ2KRewriteDecodesAlikeWithOpenJPEG`, `TestJ2KFreeWordsDecodeAlikeWithOpenJPEG`: a CPF and a CRG holding a name planted after SIZ, removed, and decoded alike) |
| `NOPACS_CHECK_DICOMWEB_PYTHON` | The Python, with dicomweb-client and pydicom, that checks a tenant's DICOMweb services from a client that is not NoPACs' (`scripts/check/dicomweb-client.py`): it searches every study, series and instance of an archive made by reception (QIDO-RS, paging through the results), retrieves each instance as stored, its metadata, its bulk data and its first and last frame (WADO-RS), and compares each with pydicom's reading of the file received. The key reaches the script in its environment, never its command line. dicomweb-client is not in the base environment; install it into a virtual environment (`python3 -m venv`, then `pip install dicomweb-client pydicom`). Set, even to nothing, it means the check was asked for and a missing interpreter fails. `NOPACS_PYDICOM_CORPUS` adds files to the archive. | unset: the test skips | `internal/api` (`TestDICOMwebWithDicomwebClient`); set by `make check-dicomweb` |
| `NOPACS_CHECK_STOW_PYTHON` | The Python, with dicomweb-client and pydicom, that checks the STOW-RS receiver from a client that is not NoPACs' (`scripts/check/stow-dicomweb-client.py`), in a real server process: it stores the public CT study with dicomweb-client and checks the answer (each instance with its WADO-RS Retrieve URL; an instance of another study refused 409 with A900H), then, once the reception is done, compares each archived file with the bytes sent, with hashlib against its scan's manifest and as a data set with pydicom. The key reaches the script in its environment, never its command line. Install dicomweb-client into a virtual environment as for `NOPACS_CHECK_DICOMWEB_PYTHON`. Set, even to nothing, it means the check was asked for and a missing interpreter fails. | unset: the test skips | `internal/servecmd` (`TestSTOWWithDicomwebClient`); set by `make check-stow` |
| `NOPACS_DICOMWEB_KEY` | The key `scripts/check/dicomweb-client.py` sends, as `Authorization: Bearer`; given in the script's environment so it never appears on a command line. | none: the script fails without it | set by `internal/api` (`TestDICOMwebWithDicomwebClient`) and `nopacs_scripts/sections/reading_web.py` (under `scripts/lib`); read by `scripts/check/dicomweb-client.py` |
| `NOPACS_STOW_KEY` | The key `scripts/check/stow-dicomweb-client.py` stores with, given the same way. | none: the script fails without it | set by `internal/servecmd` (`TestSTOWWithDicomwebClient`) and `nopacs_scripts/sections/reading_web.py` (under `scripts/lib`); read by `scripts/check/stow-dicomweb-client.py` |
| `NOPACS_PYDICOM_CORPUS` | pydicom's `test_files` directory, from which the metadata comparison also receives implicit VR, big endian, padded, JPEG-LS and explicit big endian files, and whose files with pixel data the frame table comparison adds. | unset: only the files of `internal/receive/testdata` | `internal/api` (`TestMetadataEqualsPydicom`, `TestCatalogEqualsPydicom`, `TestDICOMwebWithDicomwebClient`), `internal/receive/frames` (`TestTableEqualsPydicom`), `internal/deid/engine` (`TestCorpusFilesAreDeidentified`), `internal/deid/check` (`TestAgreesWithPydicom`), `internal/receive` (`TestCorpusThroughADeidentifyingReceiver`); set from `PYDICOM_CORPUS=` by `make check-metadata`, `make check-catalog`, `make check-dicomweb` and `make check-frames` |
| `NOPACS_CHECK_TENANTS` | `1` when the server under check is the real program, which makes the server check go on to tenants and receptions. | unset | `scripts/check/server.sh` sets it, `scripts/check/server.py` reads it |
| `NOPACS_CHECK_ROOT` | The directory root the real server was given as `fast`, for the reception calls of the server check. | none; required with `NOPACS_CHECK_TENANTS=1` | `scripts/check/server.sh`, `scripts/check/server.py` |
| `NOPACS_SAMPLE_DIR` | A directory the sample-writing test uses as its data directory, for a tool such as `sqlite3` to read back. | unset: the test skips | `internal/driver/sqlite`, `internal/driver/postgres`, `internal/registry`; set by `scripts/check-master-*.sh` and `scripts/check-tenant-*.sh` |
| `NOPACS_SAMPLE_SCHEMA` | The name of a Postgres schema to make for the sample-writing test (it must not exist). | unset: the test skips | `internal/driver/postgres`; set by `scripts/check/master-postgres.sh`, `scripts/check/tenant-postgres.sh` |
| `NOPACS_S3_OUTSIDE_MANIFEST` | A file the S3 driver's test writes the keys, sizes and SHA-256 of what it put into, for an outside reader to compare. | unset: the test skips | `internal/blob/s3` (`TestWriteForOutsideCheck`) |
| `NOPACS_SERVED_ANSWERS` | A file the test of refused requests writes the served method's answers into, so two builds can be compared. | unset: nothing is written | `internal/server` (`TestNotServedRefusedAsServed`) |

### The proof script

| Variable | What it does | Default | Read by |
|---|---|---|---|
| `NOPACS_PROVE_STRICT` | `1`: a check of the proof that is skipped is a failure, as in CI. | `0` | `scripts/prove/run.sh`; set to `1` by `make ci` on Linux and by CI, not by `make prove` itself |
| `NOPACS_CLOCK_OFFSET` | A fixed offset (`720h`, `400d`, `-36h`) added to the time of a server built with `-tags clock`, read once at its start; a normal build never reads it. An offset it cannot read ends the process. | unset: no offset | `internal/clock`; set by the proofs that move time |
| `NOPACS_CLOCK_FILE` | A file holding an offset for a server built with `-tags clock`, read at most every 200 ms, so a proof moves a running server's time by rewriting it; it wins over `NOPACS_CLOCK_OFFSET` once it exists. A normal build never reads it. | unset | `internal/clock`; written by `scripts/prove/clock_offset.py` |
| `NOPACS_PROVE_S3_SECRET` | The secret key the `versions` and `room` groups of `make prove-whole` hand the command line for the S3 server they start in a container (the command line reads a storage's secret from this variable, never from an argument). Set by those groups for the commands they run; nobody sets it by hand. | unset | `scripts/lib/nopacs_scripts/wholemore.py` |
| `NOPACS_PROVE_BUILD` | `host` builds only this machine's binary, for a quicker run; the check of the static build for every target is then skipped. | `all` | `scripts/prove/run.sh` |
| `NOPACS_PROVE_ONLY` | `reading` runs the builds and the sections that read the archive (13 to 19: an installation of their own built by sending public studies with dcmtk's `storescu`, then the catalog, metadata, downloads, DICOMweb and STOW-RS, the Python client and the refusals) alone, without Docker; their tenant on Postgres is then skipped. They need dcmtk (`DCMTK=` its directory) and a `PYTHON` with `scripts/requirements/check-server.txt` and `scripts/requirements/prove-reading.txt`. | unset: every section | `scripts/prove/run.sh` |
| `NOPACS_PROVE_CRASHES` | How many times the crash check kills the server while it creates a tenant. | `24` | `scripts/prove/run.sh` |
| `NOPACS_PROVE_PG_IMAGE` | The Postgres image the proof starts, when it starts its own container. | `postgres:18` | `scripts/prove/run.sh` |
| `NOPACS_PROVE_PG_CONTAINER` | A running Postgres container (such as CI's service container) the proof uses instead of starting one. Needs `NOPACS_PROVE_PG_PORT`. | none: the proof starts and removes its own | `scripts/prove/run.sh`; set by `make ci` and by CI |
| `NOPACS_PROVE_PG_PORT` | The port of that container on `127.0.0.1`. | none | `scripts/prove/run.sh`; set by `make ci` and by CI |

### Set by a test for its own child process

A test that must kill or run a real process starts its own test binary again
with these set. Nobody sets them by hand, and the test that reads one does
nothing when it is run alone.

| Variable | What it carries | Set and read by |
|---|---|---|
| `NOPACS_CLOCK_CHILD` | Marks the clock test's own child process, which checks the offset read at start | `internal/clock` (`TestEnvAtStart`) |
| `NOPACS_BLOBTEST_FLAW` | Which promise of the storage interface a deliberately flawed driver breaks | `internal/blob/suitecheck` |
| `NOPACS_BLOB_CRASH_ROOT`, `NOPACS_BLOB_CRASH_GENERATION`, `NOPACS_BLOB_CRASH_STALL` | The root, the first generation and the stall point of the child that writes until it is killed | `internal/blob/dir` |
| `NOPACS_E2E_ARGS` | The arguments of a command the end-to-end test runs as one of the two programs | `cmd/nopacs-server` |
| `NOPACS_INTERRUPT_AT` | The durable step (`scope:kind:step`, listed in `internal/interrupt/on.go`) at which a server built with `-tags interrupt` exits with status 86, as a crash there would; a name it does not know stops it at its start. A normal build never reads it | `internal/interrupt` (`TestStopsAtStep`, `TestRefusesUnknownStep`); set by `scripts/prove/interrupt.py` for the server it starts |
| `NOPACS_HEADER_QUIET_CHILD` | Makes the child read damaged files so the test can check nothing reaches standard error | `internal/receive/header` |
| `NOPACS_INCOMING_CRASH_ROOT`, `NOPACS_INCOMING_CRASH_POINT`, `NOPACS_INCOMING_CRASH_HIT` | The area's root, the step to stop at and on which hit | `internal/receive/incoming` |
| `NOPACS_KILL_CHILD_DB` | The database the child makes changes to until it is killed | `internal/driver/sqlite` |
| `NOPACS_KILL_RECEIVED_DB` | The tenant database the child records received files into, from many receptions at once, until it is killed | `internal/driver/sqlite` (`TestKillDuringReceivedBatches`) |
| `NOPACS_MIGRATE_HELPER` | Turns the test binary into one of the processes that migrate one database at once | `internal/driver/sqlite`, `internal/driver/postgres` |
| `NOPACS_OTHER_USER_DIAL` | The path of the socket the helper dials as the other user | `internal/server` |
| `NOPACS_RECEIVE_CRASH_DIR`, `NOPACS_RECEIVE_CRASH_POINT`, `NOPACS_RECEIVE_CRASH_HIT`, `NOPACS_RECEIVE_CRASH_RUN`, `NOPACS_RECEIVE_CRASH_RESEND` | The data directory, the crash point, the hit, the run and the files to send again of the pipeline's crash child | `internal/receive` |
| `NOPACS_SERVECMD_CHILD_ARGS`, `NOPACS_SERVECMD_CHILD_REAL` | The arguments of the command the child runs, and whether it serves the real handlers | `internal/servecmd` |
| `NOPACS_STANDIN_DELAY` | How long the stand-in server holds a listing whose page token is `wait`, as a Go duration (default `200ms`) | `internal/testkit/cmd/standin`, driven by `scripts/check/server.sh` |
