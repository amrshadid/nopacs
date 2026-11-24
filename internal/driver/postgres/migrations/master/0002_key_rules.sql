-- Where a key may be used from, where it was last used from, and the key a
-- rotation replaced it with; the same columns as the SQLite schema's.
--
-- networks is the key's allowed networks as one text, in the one written form
-- the settings of networks take (comma-separated, each an address or a CIDR
-- network), '' for every network. client_cert is the SHA-256 of the TLS
-- client certificate the key must come with, 64 lowercase hexadecimal digits,
-- '' for none. last_address is the caller's address written with last_used,
-- '' before the first use off the local socket. successor is the id of the
-- key that replaced this one by a rotation, '' when it was never rotated; it
-- is not a foreign key, since the successor is written in the same
-- transaction and a key is never deleted.
--
-- Each column has a default, so the rows of keys made before this migration
-- read as keys with no rule, never used from an address, never rotated.

ALTER TABLE api_keys ADD COLUMN networks     text NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN client_cert  text NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN last_address text NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN successor    text COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD CONSTRAINT api_keys_client_cert_check CHECK (client_cert = '' OR octet_length(client_cert) = 64);
