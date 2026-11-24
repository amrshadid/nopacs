"""Check the JSON that every NoPACs command writes with --json, with Python.

The Go test TestWriteJSONOutputs (internal/cli/jsonout_test.go) runs each
command with --json against an in-process server and leaves its standard output
in <dir>/<name>.json. This script, which shares no code with NoPACs, reads
each file with Python's json module and checks what a program that reads the
output relies on:

  * the file is exactly one JSON document: json.load refuses trailing text, so
    a note or a second document on standard output fails here;
  * the document is an object whose first key is "schemaVersion", equal to 1;
  * every command of the grammar left a document, and no file is unexpected,
    so a command added without a check, or dropped without one, is noticed;
  * a list command's document holds its array under its field and a boolean
    "more";
  * no document holds the database address the test gave "tenant create",
    which no answer may echo.

Usage: cli-json.py <dir>. Any disagreement exits with status 1 and a reason
on standard error.
"""

import json
import os

from nopacs_scripts.report import failing

# The documents the test writes, and for a list the field that holds its array.
EXPECTED = {
    "storage-add": None, "storage-add-s3": None, "storage-list": "storages", "storage-show": None,
    "storage-set": None, "storage-test": None, "storage-remove": None,
    "receiver-add": None, "receiver-list": "receivers", "receiver-show": None, "receiver-set": None, "receiver-remove": None,
    "reception-list": "receptions", "reception-show": None, "reception-retry": None, "reception-discard": None,
    "quarantine-list": "files", "quarantine-show": None, "quarantine-release": None, "quarantine-reject": None,
    "correction-delete-preview": None, "correction-delete": None, "correction-list": "corrections", "correction-show": None,
    "correction-undo": None, "correction-delete-again": None, "correction-purge": None, "correction-allow": None,
    "correction-relabel": None, "correction-replace": None,
    "profile-create": None, "profile-get": None, "profile-list": "profiles", "profile-versions": "versions",
    "profile-export": None, "profile-validate": None, "profile-describe": None, "profile-try": None,
    "profile-explain": None, "profile-diff": None,
    "profile-try-local": None, "profile-lookup-import": None, "profile-lookup-list": "tables",
    "deid-code": None, "receiver-route-export": None, "receiver-route-import": None,
    "receiver-add-import": None, "job-import": None, "job-files": "files", "job-units": "units",
    "job-scrub": None, "job-findings": "findings", "job-finding": None, "job-rebuild-compare": None,
    "version": None,
    "whoami": None,
    "tenant-create": None,
    "tenant-create-postgres": None,
    "tenant-list": "tenants",
    "tenant-show": None,
    "tenant-status": None,
    "tenant-remove": None,
    "key-create": None,
    "key-list": "keys",
    "key-show": None,
    "key-revoke": None,
    "grant-set": None,
    "grant-list": "grants",
    "grant-remove": None,
    "setting-get": None,
    "setting-set": None,
    "setting-unset": None,
    "setting-list": "settings",
    "audit-list": "entries",
    "audit-verify": None,
    "audit-checkpoints": "checkpoints",
    "audit-export": None,
    "audit-public-key": None,
    "context-add": None,
    "context-list": "contexts",
    "context-show": None,
    "context-use": None,
    "context-remove": None,
}


fail = failing("check-json: ")


def main(argv):
    if len(argv) != 1:
        fail("usage: cli-json.py <dir>")
    directory = argv[0]
    present = {f[: -len(".json")] for f in os.listdir(directory) if f.endswith(".json")}
    for name in sorted(set(EXPECTED) - present):
        fail("no document for " + name)
    for name in sorted(present - set(EXPECTED)):
        fail("a document nobody expects: " + name)

    for name, list_field in sorted(EXPECTED.items()):
        path = os.path.join(directory, name + ".json")
        with open(path, encoding="utf-8") as f:
            text = f.read()
        try:
            # object_pairs_hook keeps the keys in the order they were written,
            # so "first key" can be checked.
            doc = json.loads(text, object_pairs_hook=lambda pairs: pairs)
        except ValueError as error:
            fail("%s is not one JSON document: %s" % (name, error))
        if not isinstance(doc, list) or not doc:
            fail("%s is not an object" % name)
        keys = [k for k, _ in doc]
        if keys[0] != "schemaVersion" or doc[0][1] != 1:
            fail("%s does not start with schemaVersion 1: %r" % (name, doc[0]))
        if len(set(keys)) != len(keys):
            fail("%s has a repeated key: %r" % (name, keys))
        plain = json.loads(text)
        if list_field is not None:
            if not isinstance(plain.get(list_field), list):
                fail("%s has no array %s" % (name, list_field))
            if not isinstance(plain.get("more"), bool):
                fail("%s has no boolean more" % name)
        if "db.example.org" in text:
            fail("%s holds a database address" % name)
        print("ok  %-24s %s" % (name, " ".join(keys)))
    print("check-json: %d documents, each one JSON object starting with schemaVersion 1" % len(EXPECTED))

