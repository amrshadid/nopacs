// Package settingvalue reads the value of a setting that applies: on the
// installation, the installation's stored value, else the built-in default;
// on a tenant, the tenant's own value, else the installation's (none for a
// setting with no installation value), else the built-in default. Every
// value is checked against its rule as it is read, and the values the server
// acts on are returned typed, parsed by package core/settings. The API's
// handlers, the receivers' supervisor and the server's wiring of the
// reception pipeline all read a setting here, so a value means one thing
// wherever it is read.
//
// It is handed the stores it reads through Source, the one method the
// master and a tenant's database share for a stored setting, so it never
// knows which database holds a value, and a test hands it a map.
//
// # What this package must never do
//
// It must never write a setting, cache a value (a setting is read live, so
// a change applies at the next read without a restart), or decide who may
// read one: that is the settings service's. It must never open a database
// or import a driver, the handlers or the reception pipeline, which use it.
// It must never put a stored value into an error: a value that breaks its
// rule is the server's fault, named by the setting's name alone.
package settingvalue
