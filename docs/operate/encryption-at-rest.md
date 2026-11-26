# Encryption at rest

[Documentation index](../README.md) · [The security model](../concepts/security.md#secrets-at-rest) · [Replica storages](replicas.md) · [Running NoPACs](running.md)

**NoPACs does not encrypt image data itself.** The DICOM files and the records beside them
are written to a storage as they are, and the storage is what encrypts them: a bucket with
default encryption, or a disk or file system that is encrypted. What NoPACs does encrypt
with the installation key file are its own secrets (storage credentials, database passwords,
the naming, UID and date-shift secrets) and its backups
([the security model](../concepts/security.md#secrets-at-rest)).

What NoPACs adds is a **record of what each storage says about itself**, so a person can see,
in one place, which storages of a tenant are said to encrypt and which are not. It is a
statement, not a check of the disk: NoPACs makes no claim beyond what the storage or the
administrator said.

```
nopacs storage status lab1
nopacs storage status lab1 main
```

The table has an ENCRYPTED column; the one-storage form shows `Encrypted at rest` with the
algorithm where there is one.

| Storage | How the record is made | What it can say |
|---|---|---|
| an S3 bucket | the server asks the bucket for its default encryption when it opens the storage (`GetBucketEncryption`) | `yes` with the algorithm (`AES256`, `aws:kms`), `no` when the bucket says it does not encrypt new objects by default, or `unknown` |
| a directory | the administrator's own statement, `--encrypted-at-rest-declared` on `storage add` or `storage set` | `declared`, or `not declared` when no statement was made |

`unknown` for a bucket means the store could not be asked or would not say: credentials that
may not read that setting, a store that does not have it, or a storage the server has not
opened yet. It is not `no`. The algorithm shown is the name the store gave, and nothing more
(never a key's name).

```
nopacs storage set lab1 main --encrypted-at-rest-declared
```

Use it only for a volume you know is encrypted, such as a LUKS or BitLocker volume, or an
encrypted file system the site set up; the status then shows `declared`, never `yes`, since
NoPACs did not check. Anyone with the admin role on the tenant can set it, and the
change is audited like every configuration change.

## What this does and does not give

- It lets a reviewer see which storages are said to be encrypted without logging in to the
  store. It does not protect any byte.
- A bucket's default encryption applies to new objects. Objects put before it was switched
  on may not be encrypted, and the record does not say so.
- A replica is a storage with its own record. A replica on an unencrypted disk is not
  covered by the encryption of its primary, so look at every storage's row
  ([replicas](replicas.md)).
- Data in transit is covered separately: TLS on the API, and on receivers and senders that
  use it, and the TLS to an S3 store.
- NoPACs does not hold your bucket's keys and does not set a bucket's encryption, storage
  class or lifecycle.
