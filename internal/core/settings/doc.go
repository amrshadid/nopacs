// Package settings is the fixed list of settings the server knows, so an
// unknown name or a bad value is refused instead of stored where nothing
// reads it: for each, its name, its built-in default, whether a tenant may
// override it, and the rule its value must meet. The API's settings service
// and every part of the server that reads a setting look a name up here; a
// name that is not here is not a setting.
//
// The value that applies on a tenant is the tenant's override when it has
// one and the setting allows it, else the installation's value, else the
// built-in default. This package states the list and the rules; it does not
// read or store a value.
//
// Who may set a setting, and where, is its Kind (settings.proto,
// SettingsService): an ordinary setting, which a tenant's admin may override
// when it allows; an installation-only setting, such as the directories and
// endpoints storages may use; and a setting set on a tenant by the
// installation's admin only, such as the ports a tenant's receivers may use.
// The parsers of the values the reception pipeline reads (values.go) return
// typed values, so the API's handlers and the receivers read a value one
// way. Whether a value takes away what a tenant uses is not this package's
// to say: it holds no tenant's storages or receivers.
//
// # What this package must never do
//
// It must never do I/O, read the clock, or import anything of NoPACs'
// outside the core: it is part of the core (.golangci.yml, depguard "core"),
// so it is tested without a database and a new database type never changes
// it. The rules it shares with the rest of the server (the rule of a name,
// the nesting of paths) it takes from the core's packages pathname and
// ascii, so they are written once. It must never
// hold a setting whose value is a secret: a value is kept in plain text in
// the databases and answered to any reader of its scope (settings.proto: "A
// setting never holds a secret").
package settings
