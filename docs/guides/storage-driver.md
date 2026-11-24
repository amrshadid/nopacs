# Adding a storage driver

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Testing](../testing.md) · Guides: [a database driver](database-driver.md) · [an API method](api-method.md) · [a migration](migration.md) · [an event type](event-type.md)

A storage driver implements `blob.Store`: put an object whole or not at all,
read it or a range of it, list a prefix, delete with a reason recorded by the
caller, and report usage. The directory and S3 drivers are the two today.

1. **Make the package** `internal/blob/<name>`. Nothing outside it may know
   which storage it is: the reception pipeline and the handlers see only
   `blob.Store`.
2. **Meet the port's promises**, which [the port's documentation](../../internal/blob/doc.go) states and its suite
   tests: a put is whole or absent, never torn, and replaces an object whole;
   a read sees one version; keys are validated before they reach the storage;
   a listing is stable under concurrent puts; sizes are bounded.
3. **Pass the contract suite**: call `blobtest.Run` from the driver's tests
   with a `blobtest.Driver` that opens a fresh store per test, and run it in
   CI against a real server of that kind (two implementations where they
   exist, as S3 runs against Silo and RustFS).
4. **Describe the storage in the tenant port**: a storage type and its fields
   in `internal/tenant` (with their validation), its fields in the API
   contract, and its secret, if any, sealed by `internal/secret`.
5. **Open it in one place**: `internal/place/open.go` turns a tenant's storage
   row into a `blob.Store`, checking where the installation lets data go
   (a setting such as `storage.s3-endpoints`) before anything is reached.
6. **Keep its library to it**: a lint rule in `.golangci.yml`, as the S3
   driver's keeps the AWS SDK's credential chain out.
7. **Prove it**: a tenant archiving to it in `scripts/prove/reception.py`,
   with each object read back by a tool that is not NoPACs, and a backend in
   `internal/receive/bench_test.go`.
