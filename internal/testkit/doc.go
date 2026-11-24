// Package testkit holds the test helpers more than one package's tests use,
// so each is written once: a generator of DICOM files of any size from one
// public test file, streamed so a large file costs no memory; a sampler of
// the live heap for the tests that bound a reception's memory; and the
// services a test may use (a scratch Postgres schema, a scratch S3 server),
// each read from one variable, skipped when it is not set unless its
// REQUIRE variable is.
//
// # What this package must never do
//
// It must never be imported by anything the binary runs: only by tests
// (_test.go files) and by test-only programs under internal/testkit/cmd
// (.golangci.yml, depguard "testkit"). It must never hold data of a real
// person: what it makes comes from public test files the caller hands it.
// It must never decide a test's outcome by itself beyond failing it on a
// setup it cannot do (testing.TB.Fatal): the assertions are the tests' own.
package testkit
