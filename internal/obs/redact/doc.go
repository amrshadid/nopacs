// Package redact hides the fields of a protobuf message that the contract
// marks as secret, so the message can be logged.
//
// The API contract marks its six secret fields with the field option
// debug_redact (CreateTenantRequest.database_address,
// CreateKeyResponse.secret, RotateKeyResponse.secret,
// S3Storage.secret_access_key, StowSender.credential and
// TenantExportSummary.transfer_key), and the fields of a trial run that
// carry a file's values. The mark is metadata only: protobuf-go prints a
// marked field like any other, in every text form (TestUnredactedPrintsSecret
// shows it with the pinned library). This package reads the mark from each
// field's descriptor and blanks the field in a copy of the message, walking
// nested messages, repeated fields, maps and oneofs, and the message inside a
// google.protobuf.Any when its type is known. A marked string or bytes field
// that held a value reads Placeholder, so a reader of the log can see a value
// was there; any other marked field is cleared.
//
// An Any whose type is not linked into the binary cannot be walked, so its
// value is removed whole: what cannot be checked is not logged.
//
// What it must never do: change the message it is given (it works on a
// copy), or let a marked field through for any shape of message. It does not
// log anything itself.
package redact
