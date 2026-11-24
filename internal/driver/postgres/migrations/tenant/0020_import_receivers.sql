-- A receiver of a third type, import: it listens on nothing, so its port
-- is 0 and its bind address empty, and its import jobs hand it the files
-- of a directory under one of the installation's import roots. Its root,
-- layout and map are in its config JSON, as a stow receiver's keys are.
--
-- Each file an import hands in keeps, with its arrival, the parts its
-- path gave the naming rule (a JSON object, '' for none), so the pipeline
-- names the file's session and scan by them when its reception is named;
-- an arrival recorded before this migration has none.
--
-- Only checks are widened and a column with its default added, so the
-- migration is safe on a database that holds data. The rules of
-- 0002_pipeline.sql hold.

ALTER TABLE receivers DROP CONSTRAINT receivers_type_check;
ALTER TABLE receivers DROP CONSTRAINT receivers_port_check;
ALTER TABLE receivers ADD CONSTRAINT receivers_type_check CHECK (type IN ('dimse', 'stow', 'import'));
ALTER TABLE receivers ADD CONSTRAINT receivers_port_check
    CHECK ((type = 'dimse' AND port BETWEEN 1 AND 65535) OR (type IN ('stow', 'import') AND port = 0 AND bind_address = ''));

ALTER TABLE import_arrivals ADD COLUMN parts text COLLATE "C" NOT NULL DEFAULT '';
