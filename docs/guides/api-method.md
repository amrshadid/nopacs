# Adding an API method

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Testing](../testing.md) · Guides: [a database driver](database-driver.md) · [a storage driver](storage-driver.md) · [a migration](migration.md) · [an event type](event-type.md)

The API is a protobuf contract (`api/proto/nopacs/v1`), served by ConnectRPC
over HTTPS and the local socket, and called by the command line and the
interactive program.

1. **Write the method in the contract**: the request and response messages,
   each field documented, a tenant named in field 1 when the method acts on
   one. `make proto-lint` must pass, and `make proto-breaking` shows that no
   published message changed in a way that breaks a client.
2. **Generate**: `make proto`, then commit `gen/`; `make proto-check` fails when
   `gen/` differs from what the proto files make.
3. **State who may call it**: a rule in `internal/api/requirement.go` (the
   role, on the tenant or the installation, and what the audit entry
   records). A method with no rule is refused by the server.
4. **Write the handler** in `internal/api`: validate the request, reach the
   tenant only through `registry` and the tenant port, map faults through
   `internal/api/codes`, and write the audit entry with the change in one
   transaction.
5. **Test it in the method table**: `internal/api/table_test.go`
   (`TestEveryMethod`) maps every method to its cases: refusals by role, the
   invalid requests, the success, and the audit entry. Its method count says
   when a method was added without a case.
6. **Give it a command and a screen**: `internal/cli` (with the fake server in
   `internal/testkit/fakeserver`) and `internal/tui`, in the same change.
7. **Prove it from outside**: the proof's refusal matrix reads the methods from
   the proto files, so a new method is refused correctly by grpcurl, Python's
   grpcio and curl without more work; add a check of what it does.
