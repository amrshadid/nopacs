package master

import (
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/secret"
)

// ValidateKeyRef returns an Invalid error unless id could name a key a
// caller refers to: a valid id (ValidateID) of at most the length the key
// format gives every key id (secret.APIKeyIDLength), and never one that
// starts as a whole key does (secret.APIKeyPrefix). A whole key is 64
// characters an id may hold, so without this rule a person who pastes
// their key where its id goes would have it echoed in the error and kept
// in the audit log, which is hash-chained and exported and cannot be
// cleaned. The message never repeats id. name says what the value is, as
// ValidateID's does.
//
// Every operation that takes a key id from a caller (reading, revoking,
// rotating, restricting or reinstating a key, a grant by key, a filter by
// key) checks it with this, before the id reaches an error or an entry.
func ValidateKeyRef(name, id string) error {
	if strings.HasPrefix(id, secret.APIKeyPrefix) {
		return invalid("a " + name + " is the part of a key after " + secret.APIKeyPrefix + " and before the next _; a whole key is a secret and is never given as an id")
	}
	if len(id) > secret.APIKeyIDLength {
		return invalid("a " + name + " is at most " + strconv.Itoa(secret.APIKeyIDLength) + " characters; a longer value is not a key id")
	}
	return ValidateID(name, id)
}

// NotAKeyID is what KeyObject records in place of a value that is not a
// key id. Its spaces and parentheses are outside an id's characters, so it
// can never be read as the id of a real key.
const NotAKeyID = "(not a key id)"

// KeyObject returns id as an audit entry's object or detail may hold it:
// id itself when ValidateKeyRef accepts it, "" for none, and NotAKeyID
// otherwise, so a whole key, or anything else a caller sent in its place,
// never reaches the log.
func KeyObject(id string) string {
	switch {
	case id == "":
		return ""
	case ValidateKeyRef("key id", id) != nil:
		return NotAKeyID
	}
	return id
}
