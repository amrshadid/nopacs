-- The tenant's own secrets, such as the key of the keyed hash its naming
-- rules use, each sealed with the installation key by the server before it
-- is stored: this database never holds a plaintext secret, and a copy of it
-- opens nothing without the installation's key file. A secret is written
-- once and never replaced, since what was made with it depends on it.
CREATE TABLE tenant_secrets (
    name    text        NOT NULL PRIMARY KEY,
    sealed  bytea       NOT NULL CHECK (length(sealed) BETWEEN 1 AND 1024),
    created timestamptz NOT NULL
);
