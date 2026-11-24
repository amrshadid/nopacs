-- A receiver now either de-identifies what it receives with a profile
-- version or archives it as received, and the tenant setting
-- receiver.allow-as-received decides whether an as-received receiver may
-- exist in the tenant. Its built-in default is "false", so a tenant made
-- from now on holds only receivers that de-identify unless the
-- installation's admin says otherwise. A tenant that already has a receiver
-- keeps what its receivers did: every receiver stored before this migration
-- archives as received, so the tenant is given the setting "true" here,
-- which the installation's admin can unset. A tenant with no receiver yet
-- has nothing to keep and takes the default. The row is added only when
-- the tenant has none of that name, so the migration changes no value a
-- person set.
INSERT INTO settings (name, value)
    SELECT 'receiver.allow-as-received', 'true'
    WHERE EXISTS (SELECT 1 FROM receivers)
      AND NOT EXISTS (SELECT 1 FROM settings WHERE name = 'receiver.allow-as-received');

-- How each received file and quarantine record was recorded: true at a
-- receiver that de-identifies, whose rows hold the keyed remapping of the
-- file's placing UIDs and a keyed checksum, false as received. Every row
-- written before is false, since no receiver de-identified before.
ALTER TABLE received_files ADD COLUMN deidentified boolean NOT NULL DEFAULT false;
ALTER TABLE quarantine ADD COLUMN deidentified boolean NOT NULL DEFAULT false;
