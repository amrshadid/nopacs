-- A tenant may now be moved (an export handed it to another installation)
-- or fenced (the owner record on one of its storages names another
-- installation or a later generation): read only, its reads served.
--
-- The check is replaced in one transaction: every row already holds one of
-- the old states, all of which the new check allows, so the new constraint
-- is checked once against them and nothing a tenant holds changes.
ALTER TABLE tenants DROP CONSTRAINT tenants_state_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_state_check
    CHECK (state IN ('ready', 'unreachable', 'upgrading', 'failed', 'moved', 'fenced'));
