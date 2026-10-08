-- Finalize schema hygiene after the smart-health and synthetic-data features
-- were retired. The migration runner wraps this file and its checksum record
-- in one transaction, so no BEGIN/COMMIT is used here. Old migration files,
-- their checksums and their schema_migrations records are left untouched.
-- Every statement is idempotent. Non-empty legacy tables are kept as they are.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

-- Two retired tables with no runtime reference are dropped only when they are
-- empty. The emptiness check runs under an ACCESS EXCLUSIVE lock in the same
-- transaction, so no row can be written between the check and the drop.
-- RESTRICT never cascades: an unexpected dependent object fails the migration.
DO $migration$
BEGIN
    IF to_regclass('public.billing_usage_entries') IS NOT NULL THEN
        LOCK TABLE public.billing_usage_entries IN ACCESS EXCLUSIVE MODE;
        IF NOT EXISTS (SELECT 1 FROM public.billing_usage_entries) THEN
            DROP TABLE IF EXISTS public.billing_usage_entries RESTRICT;
        END IF;
    END IF;

    IF to_regclass('public.groups_video_price_backup_220') IS NOT NULL THEN
        LOCK TABLE public.groups_video_price_backup_220 IN ACCESS EXCLUSIVE MODE;
        IF NOT EXISTS (SELECT 1 FROM public.groups_video_price_backup_220) THEN
            DROP TABLE IF EXISTS public.groups_video_price_backup_220 RESTRICT;
        END IF;
    END IF;
END
$migration$;

-- Six retired columns with no runtime reference: the synthetic empty-bucket
-- switch, four unused ops error-log columns, and the archived_at timestamp of
-- the long-term billing dedup archive. Other columns are not touched.
ALTER TABLE IF EXISTS public.channel_monitor_v2_config
    DROP COLUMN IF EXISTS synthetic_empty_buckets_enabled RESTRICT;

ALTER TABLE IF EXISTS public.ops_error_logs
    DROP COLUMN IF EXISTS attempted_key_prefix RESTRICT,
    DROP COLUMN IF EXISTS deleted_key_name RESTRICT,
    DROP COLUMN IF EXISTS network_error_type RESTRICT,
    DROP COLUMN IF EXISTS provider_error_code RESTRICT;

ALTER TABLE IF EXISTS public.usage_billing_dedup_archive
    DROP COLUMN IF EXISTS archived_at RESTRICT;
