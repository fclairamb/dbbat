-- Deliberately a no-op.
--
-- The up-migration repairs a data inconsistency; there is no honest way to put
-- it back:
--
--   * The link it overwrote is not recorded anywhere. Rolling back would mean
--     re-deriving the *wrong* definition a buggy backfill picked, which is not
--     information this schema keeps — and re-introducing a broken link on
--     purpose is not what a rollback is for.
--   * The inactive stand-ins it may have synthesized cannot simply be deleted:
--     access_grants.grant_definition_id points at them and is NOT NULL, so the
--     delete would fail (or orphan grants). They carry the
--     'legacy-grant-shape-' prefix, so the down-migration of
--     20260806020000_grants_reference_definitions removes them as part of the
--     full rollback that also drops the column referencing them — which is the
--     only point at which removing them is coherent.
--
-- Re-applying the up-migration after this is harmless: it is idempotent and a
-- no-op once nothing is mismatched.
DO $$
BEGIN
    -- nothing to undo
END
$$;
