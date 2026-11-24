# Install the two programs

[Documentation index](../README.md) · [A first archive](first-archive.md) · [Running NoPACs](../operate/running.md)

NoPACs is two programs, built from one source and carrying one version:

- **`nopacs-server`** is the server. It holds the archive, receives DICOM, and
  answers the API. It also has the few commands that must touch the data
  directory while the server is stopped (`init`, `serve`, `backup`,
  `tenant import`). It runs on the machine that holds the data.
- **`nopacs`** is the command line, and, run with no arguments in a terminal,
  an interactive program. It is a client of the API and links no database or
  storage driver, so it can sit on an analyst's laptop with nothing of the
  archive on it.

Both are static programs with no libraries to install.

## What exists today

There is no release yet: you build the programs from a checkout of the source.

## Build

You need Go 1.26 or later and nothing else (no C compiler: the programs are pure Go).

```sh
CGO_ENABLED=0 go build -o bin/ ./cmd/nopacs ./cmd/nopacs-server
bin/nopacs version
bin/nopacs-server version
```

The two `version` lines are the same. `make build VERSION=v0.1.0` builds both
for Linux (amd64, arm64 and 386), macOS (arm64) and Windows (amd64), with the
version stamped in, into `bin/`.

Put the programs where your `PATH` finds them (`/usr/local/bin`, say).

## Where each goes

| Machine | Install | Why |
|---|---|---|
| The server's host | `nopacs-server`, and `nopacs` too if you want to run commands there | The server needs its data directory on a disk it owns. On the host, `nopacs` reaches the server on a local socket and needs no key. |
| An analyst's or an operator's computer | `nopacs` only | It reaches the server over the network with a key. See [the command line](../use/command-line.md#contexts) for naming several installations. |

## What the server needs

- **A data directory** that only the user who runs the server can open. `init`
  makes it private (mode 0700). It holds the key file, the master database, a
  SQLite database for each tenant, the incoming area (where a file waits until
  it is archived) and `run/`. Keep its path short: the local socket lives in
  `run/` and a Unix socket's path is limited to about a hundred characters, so
  a longer path makes every command fail with `connect: invalid argument`
  ([troubleshooting](../operate/troubleshooting.md)).
- **Room for the archive**, on a disk or in an S3 bucket the installation is
  told about. The archived files are not in the data directory.
- **A certificate and its key**, in PEM files, for the network listener. Without
  `--listen` the server answers only on its local socket; with `--listen` it
  serves TLS, and plain HTTP only on a loopback address.
- **Postgres**, for a production installation (see [choosing a
  database](../operate/database.md)). The first trial needs nothing: SQLite is
  in the program.

## Check it

```sh
nopacs-server --help
nopacs --help
```

Every command both programs have, with its flags, is in [the command
reference](../reference/commands.md). Then make [a first archive](first-archive.md).
