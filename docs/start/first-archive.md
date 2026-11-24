# A first archive, end to end

[Documentation index](../README.md) · [Install the two programs](install.md) · [Concepts](../concepts/archive.md) · [Running NoPACs](../operate/running.md)

This page takes you from nothing to one scan in an archive, de-identified on the
way in, and reads it back. It uses one machine, the local socket (so no key) and
one test file. Allow ten minutes.

You need the two programs ([install](install.md)), and one DICOM file to send
with a DICOM sender such as dcmtk's `storescu`. **Use a synthetic or public test
file, never a real patient's**: you are making a trial installation, and you have
not yet checked a profile on your own data.

Words used here are explained in [the concepts page](../concepts/archive.md): an
*installation* is one server with its data; a *tenant* is a separate archive
inside it; a *scan* is one series of images, kept under a *session* (one study)
under a *subject* (one patient, named by a label the archive makes).

## 1. Make the installation and start the server

```sh
export NOPACS_DATA_DIR=$HOME/nopacs-trial/data
nopacs-server init
nopacs-server serve
```

`init` makes the data directory, the key file and the master database, and
prints where the key file is. Copy that file somewhere safe even for a trial: it
seals every secret the installation keeps, and nothing can bring them back
without it. `serve` runs in the foreground and answers on a local socket in the
data directory; leave it running and open a second terminal for the rest, with
`NOPACS_DATA_DIR` set the same way. Stop it with Ctrl-C when you are done.

On the local socket no key is needed: a command is the user that runs the server.

## 2. Make a tenant, and say where it may keep data

```sh
mkdir -p $HOME/nopacs-trial/archive
nopacs tenant create lab1
nopacs setting set storage.directory-roots fast=$HOME/nopacs-trial/archive
nopacs setting set receiver.ports 11112 --tenant lab1
```

Where a tenant's data may go is the installation's decision, not the tenant's.
The first `setting` names a *root*, a directory a tenant's storages may lie under;
the second allots the tenant a port its receivers may listen on. Until both are
set, a tenant can have neither storage nor receiver.

## 3. A profile, a storage and a receiver

A receiver admits only the callers it lists, and by default it de-identifies
every file before anything is stored. A de-identification *profile* says how.
The simplest is the standard's basic profile, which the server ships:

```sh
cat > research.json <<'DOC'
{"bases": ["annex-e/basic:2026d"], "rules": []}
DOC
nopacs profile create lab1 research --document research.json --note "basic profile"
nopacs storage add lab1 main --type dir --root fast --path main
nopacs storage test lab1 main
nopacs receiver add lab1 scanner1 --storage main --port 11112 --aet NOPACS \
    --allow-aet SCANNER --allow-from 127.0.0.1/32 --profile research@1 \
    --quiet-period 5s --enabled
```

`storage test` writes, reads back, compares and removes a small object, and says
what failed if it did. The receiver listens on port 11112 as the application
entity `NOPACS`, and admits only a sender whose calling AE title is `SCANNER`
connecting from this machine. The quiet period is how long a study must go
without a file before its reception is complete; the default is a minute, and
five seconds (the shortest) is only for a trial.

## 4. Send a file

```sh
storescu -aet SCANNER -aec NOPACS 127.0.0.1 11112 test-file.dcm
```

Wait a few seconds, then watch the reception:

```sh
nopacs reception list lab1
nopacs tenant status lab1
```

The reception passes through *receiving*, *complete*, *identified*, *archived*
and *verified* to *done* ([a reception, end to end](../use/receiving.md)). If a
file cannot go on, it is held in *quarantine* with the reason, never lost and never
archived as it came: see `nopacs quarantine list lab1`.

## 5. Read it back

```sh
nopacs catalog subjects lab1
nopacs catalog scans lab1
```

You will see one subject with a label such as `sub-u7ltdkd4c3hufknf`, one session and one
scan. The labels are made from a keyed hash, so they are not the patient's name or
identifier. Then, with the labels you got:

```sh
nopacs scan show lab1 SUBJECT/SESSION/SCAN --metadata
nopacs download lab1 SUBJECT --to ./downloaded
```

`scan show --metadata` prints each instance's DICOM header as JSON: the name and the
patient identifier are gone, as the profile says. `download` writes the files
under `downloaded/<subject>/<session>/<scan>/`, each checked against the
checksum the server announced before it is renamed into place. You can read a
file with any DICOM tool to confirm that what is archived is what the profile
leaves.

## What to do next

- Give a person or a program a key, and reach the server over TLS from another
  machine: [running NoPACs](../operate/running.md#the-first-key).
- Change what is removed, kept or replaced: [profiles](../use/profiles.md).
- Understand what you made: [concepts](../concepts/archive.md).
- Send what you hold on to another system: [sending](../use/sending.md).

## What this trial leaves out

A single machine, a directory for storage, SQLite for every database, no TLS and no
key. Those are for a trial. A site that keeps data for others uses Postgres for the
databases ([choosing a database](../operate/database.md)), TLS and keys for the
network, and backs the data directory up ([backup](../operate/backup.md)).
