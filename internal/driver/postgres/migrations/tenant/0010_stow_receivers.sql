-- A receiver of a second type, stow: it takes instances stored over HTTP
-- into the tenant's DICOMweb service (PS3.18 10.5) and listens on no port
-- of its own, so its port is 0 and its bind address empty. The keys it
-- takes requests from and its largest request are in its config JSON, as a
-- DIMSE receiver's callers are. The stow receivers share port 0, so a
-- change of one reads the others by receivers_by_port, to refuse a key two
-- of them list.
--
-- The rules of 0002_pipeline.sql hold.

ALTER TABLE receivers DROP CONSTRAINT receivers_type_check;
ALTER TABLE receivers DROP CONSTRAINT receivers_port_check;
ALTER TABLE receivers ADD CONSTRAINT receivers_type_check CHECK (type IN ('dimse', 'stow'));
ALTER TABLE receivers ADD CONSTRAINT receivers_port_check
    CHECK ((type = 'dimse' AND port BETWEEN 1 AND 65535) OR (type = 'stow' AND port = 0 AND bind_address = ''));
