-- Who a private app is published to.
--
-- An app carries a visibility: `public`, which every platform may be offered,
-- or `private`, which only named ones may. Until now the catalogue builder read
-- that field and did the only thing it could with it — `WHERE visibility =
-- 'public'` — so a private app was published to nobody. These two tables are
-- the missing half: a platform this registry can recognise, and the grant that
-- says which private apps it may see.
--
-- **This is the first schema the App Store owns.** Every table it has used so
-- far — store_apps, store_app_versions, store_catalog_snapshots — was created
-- by the core's migration 00038 and stayed there when this product left, for
-- the reason recorded in the core's CHANGELOG: those migrations had run on
-- every deployment in the field, and removing an applied migration buys a tidy
-- directory at the price of a history that no longer describes the database.
--
-- New schema is different. A table for a feature only the App Store has does
-- not belong in a migration every deployment of the platform runs, so this
-- history is this repository's, applied with MIGRATIONS_DIR and its own
-- MIGRATIONS_TABLE — which is exactly what the core's cmd/migrate grew that
-- pair of variables for. The one thing it must never do is reach into a table
-- the core owns, because the core's history knows nothing about this file.
-- Nothing here does; see below for how the snapshot cache is kept out of it.

-- +goose Up

-- A deployment this registry can recognise.
--
-- Not a tenant and not a user: a *platform* — one Nexus installation in the
-- field, which reaches this registry with no session at all. The catalogue
-- endpoint is public by design (an instance pulling a catalogue has nobody to
-- be), so the only thing it can present is a credential of its own.
CREATE TABLE IF NOT EXISTS store_platforms (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- What a human calls it in the console: "Дархан-Уул аймаг", "Salus".
    name        VARCHAR(255) NOT NULL,
    -- Where it answers, when it is known. Informational: identity is the
    -- token, not the address, because an address is not a secret and a
    -- deployment behind a proxy does not know its own.
    origin      TEXT NOT NULL DEFAULT '',
    -- The credential, stored as a SHA-256 digest of the token this registry
    -- issued. The token itself is shown once, at issue, and never again: a
    -- registry that can read back the credentials of a hundred deployments is
    -- a registry whose database is worth stealing for that alone.
    token_digest CHAR(64) NOT NULL UNIQUE,
    -- Revocation without deletion. A disabled platform keeps its grants and
    -- its history, and simply stops being recognised; deleting the row would
    -- also delete the record of what it had been allowed to see.
    disabled_at TIMESTAMPTZ,
    -- When the credential was last accepted. It is what tells an operator
    -- whether a deployment they issued a token to has ever actually used it.
    last_seen_at TIMESTAMPTZ,
    note        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by  UUID
);

-- Which private apps a platform may be offered.
--
-- Only private apps need a row: a public app is offered to everyone and a grant
-- for one would be a row that means nothing, quietly becoming wrong the moment
-- the app's visibility changed. The catalogue builder reads this as "and also
-- these", never as "only these".
CREATE TABLE IF NOT EXISTS store_app_grants (
    app_id      VARCHAR(128) NOT NULL REFERENCES store_apps(id) ON DELETE CASCADE,
    platform_id UUID NOT NULL REFERENCES store_platforms(id) ON DELETE CASCADE,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    granted_by  UUID,
    PRIMARY KEY (app_id, platform_id)
);

CREATE INDEX IF NOT EXISTS idx_store_app_grants_platform ON store_app_grants(platform_id);

-- The snapshot cache, per audience.
--
-- store_catalog_snapshots is the core's table, keyed by (channel, platform),
-- and this history must not alter it. So the private catalogues are cached
-- here instead, in a table this repository owns, with the same columns plus the
-- platform they were built for. The public catalogue keeps using the core's
-- table and its behaviour is untouched.
--
-- Cached at all because the bytes are what the signature covers: the endpoint
-- serves exactly what was signed rather than re-encoding, and rebuilding per
-- request would make that property depend on Go's JSON encoder producing
-- identical output for ever. The failure if it ever did not would be silent —
-- every instance rejecting the signature and simply never updating again.
CREATE TABLE IF NOT EXISTS store_private_snapshots (
    platform_id  UUID NOT NULL REFERENCES store_platforms(id) ON DELETE CASCADE,
    channel      VARCHAR(16) NOT NULL,
    platform     VARCHAR(32) NOT NULL,
    revision     BIGINT NOT NULL,
    generated_at TIMESTAMPTZ NOT NULL,
    etag         VARCHAR(80) NOT NULL,
    document     BYTEA NOT NULL,
    built_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (platform_id, channel, platform)
);

-- The application role is what the platform switches into for tenant-scoped
-- work (see the core's dbguard), so it needs the same grants here that 00038
-- gave the tables beside these.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gerege_nexus_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON
            store_platforms, store_app_grants, store_private_snapshots
            TO gerege_nexus_app;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down

DROP TABLE IF EXISTS store_private_snapshots;
DROP TABLE IF EXISTS store_app_grants;
DROP TABLE IF EXISTS store_platforms;
