-- Clear runtime data that no code path reads any more. The migration runner
-- wraps this file and its checksum record in one transaction, so no
-- BEGIN/COMMIT is used here. Every statement is idempotent. No table, column
-- or index is dropped.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

-- Soft-deleted accounts keep their rows for billing history, but nothing reads
-- their credentials. Account deletion clears them from now on; this clears the
-- rows deleted before that change.
UPDATE accounts
SET credentials = '{}'::jsonb
WHERE deleted_at IS NOT NULL
  AND credentials <> '{}'::jsonb;

-- Channel monitor v2 stops aggregating and pruning while it is disabled, so its
-- samples stay frozen. They are cleared only while it is disabled; the config
-- row and the aggregation watermarks are kept. TRUNCATE without CASCADE fails
-- the migration if any other object references these tables.
DO $migration$
BEGIN
    IF to_regclass('public.channel_monitor_v2_config') IS NOT NULL
       AND EXISTS (SELECT 1 FROM public.channel_monitor_v2_config WHERE id = 1 AND NOT enabled) THEN
        TRUNCATE TABLE
            public.channel_monitor_v2_metrics_1m,
            public.channel_monitor_v2_metrics_rollup,
            public.channel_monitor_v2_error_metrics_1m,
            public.channel_monitor_v2_error_metrics_rollup,
            public.channel_monitor_v2_user_metrics_1m,
            public.channel_monitor_v2_user_metrics_rollup,
            public.channel_monitor_v2_latency_histograms_1m,
            public.channel_monitor_v2_latency_histograms_rollup;
    END IF;
END
$migration$;
