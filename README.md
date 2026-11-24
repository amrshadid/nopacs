# NoPACs

A research imaging archive in one Go module, built as two static binaries: the
server, `nopacs-server`, and the command line, `nopacs`. It receives DICOM,
de-identifies it, routes it to a storage, catalogs it as subject / session /
scan, and serves it to other systems through an API with keys, as a core that
research platforms are built on.

NoPACs was formerly named CORIA.

**State: early development, no release yet.** What runs today:

- an installation made with `nopacs-server init` and served with
  `nopacs-server serve`, over TLS and a local socket;
- tenants, each with its own database on SQLite or Postgres; keys, grants and
  an audit log of every request;
- DICOM receivers per tenant, closed to callers they do not list, and a STOW-RS
  receiver for keys it lists, that archive each study to a directory or an S3
  bucket as subject / session / scan, with a manifest and checksums, verified
  against what was sent; a quarantine for what cannot be archived, with its
  reason; a crash at any step loses no file a sender was told was received;
- the archive read back: a catalog of subjects, sessions and scans, each
  instance's DICOM header as JSON, the files checked against their manifest as
  they stream, and DICOMweb (QIDO-RS, WADO-RS, STOW-RS) under
  `/tenants/<name>/dicom-web`;
- a status and a live event stream per tenant;
- de-identification by profile (a receiver de-identifies what it receives, or
  keeps it as received when told so), checked again after, with jobs that check
  or dry-run archived data against a profile;
- sending archived studies on to other systems (DIMSE and STOW-RS), importing a
  directory, corrections (delete, relabel, replace) with a record, a backup and
  restore, and moving a tenant to another installation;
- a command line and an interactive program for all of it, and generated
  clients for Python (`clients/python`) and TypeScript (`clients/typescript`).

Not yet built: replication between installations and retention (their API is
defined and not served), and a backup made through the API (`nopacs-server backup create` makes one on the
server's machine). Until you have checked a profile on test data with `nopacs
profile try`, send only synthetic or public test data.

## Build

NoPACs needs Go 1.26 or later and nothing else: it is pure Go and builds with
`CGO_ENABLED=0` into static binaries.

```sh
go build ./cmd/nopacs ./cmd/nopacs-server
./nopacs version
./nopacs-server version
```

`nopacs` is the command line, a client of the API that links no database or
storage driver, so it can sit on an analyst's laptop. `nopacs-server` runs the
installation and holds the commands that must touch the data directory while
the server is stopped (`init`, `serve`, `backup`, `tenant import`). Both print
the same version.

`make build VERSION=v0.1.0` builds both for every target in `bin/` with the
version stamped in.

## Run

```sh
nopacs-server init --data-dir ./data     # key file, master database; prints where the key file is
nopacs-server serve --data-dir ./data    # the server, on the local socket in ./data/run
nopacs tenant create lab1 --data-dir ./data   # a client: any other terminal
```

With no key, the `nopacs` commands reach the server on its local socket as the user that
runs it; over the network (`--listen`, `--tls-cert`, `--tls-key`, and a key on
the client) they need a key. [Running NoPACs](docs/running.md) goes on from here
to the storages, receivers and keys, and [the command line](docs/command-line.md)
lists every command.

## Documentation

[docs/README.md](docs/README.md) is the index. In short:

| If you | Start with |
|---|---|
| run NoPACs | [Running NoPACs](docs/running.md), [the command line](docs/command-line.md), [settings](docs/settings.md) |
| build on its API | [Receiving data through the API](docs/api-walkthrough-reception.md), [reading the archive](docs/reading.md); the contract is in `api/proto/nopacs/v1` |
| extend the code | [Architecture](docs/architecture.md), [the guides](docs/README.md#if-you-extend-the-code), [testing](docs/testing.md), [gates and outside checks](docs/gates.md) |

Each package's own documentation is its `doc.go` (`go doc ./internal/receive`);
the pages in `docs/` hold what no single package can say.

## Branches

`develop` is where work goes, pushed once the gates pass; `main` holds releases
and takes changes from `develop` only.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
