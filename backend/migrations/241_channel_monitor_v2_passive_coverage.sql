ALTER TABLE channel_monitor_v2_config
    ADD COLUMN IF NOT EXISTS group_scope_mode TEXT;

-- A nullable first step lets a rerun distinguish rows that still need the
-- legacy group_ids interpretation from rows already migrated to selected=[].
UPDATE channel_monitor_v2_config
SET group_scope_mode = CASE WHEN cardinality(group_ids) > 0 THEN 'selected' ELSE 'all' END
WHERE group_scope_mode IS NULL;

ALTER TABLE channel_monitor_v2_config
    ALTER COLUMN group_scope_mode SET DEFAULT 'all',
    ALTER COLUMN group_scope_mode SET NOT NULL,
    ADD COLUMN IF NOT EXISTS synthetic_empty_buckets_enabled BOOLEAN NOT NULL DEFAULT FALSE;

DO $migration$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'channel_monitor_v2_config_group_scope_mode_check'
          AND conrelid = 'channel_monitor_v2_config'::regclass
    ) THEN
        ALTER TABLE channel_monitor_v2_config
            ADD CONSTRAINT channel_monitor_v2_config_group_scope_mode_check
            CHECK (group_scope_mode IN ('all', 'selected'));
    END IF;
END
$migration$;

-- Remove IDs that were deleted before this migration. An empty selected list
-- remains selected and therefore continues to represent an empty scope.
UPDATE channel_monitor_v2_config config
SET group_ids = COALESCE(
        ARRAY(
            SELECT configured_group_id
            FROM unnest(config.group_ids) AS configured(configured_group_id)
            WHERE EXISTS (
                SELECT 1 FROM groups g
                WHERE g.id = configured.configured_group_id
                  AND g.deleted_at IS NULL
            )
        ),
        '{}'::BIGINT[]
    ),
    version = config.version + 1,
    updated_at = NOW(),
    updated_by = NULL
WHERE config.group_scope_mode = 'selected'
  AND config.group_ids IS DISTINCT FROM COALESCE(
        ARRAY(
            SELECT configured_group_id
            FROM unnest(config.group_ids) AS configured(configured_group_id)
            WHERE EXISTS (
                SELECT 1 FROM groups g
                WHERE g.id = configured.configured_group_id
                  AND g.deleted_at IS NULL
            )
        ),
        '{}'::BIGINT[]
    );

-- Keep the first full object for each supported platform in its existing
-- relative order; append newly supported platforms in the backend catalog order.
WITH catalog(platform, sort_order) AS (
    VALUES
        ('anthropic', 1),
        ('openai', 2),
        ('gemini', 3),
        ('antigravity', 4),
        ('grok', 5),
        ('kimi', 6),
        ('zhipu', 7),
        ('deepseek', 8),
        ('minimax', 9),
        ('opencode_go', 10),
        ('composite', 11)
),
existing AS (
    SELECT DISTINCT ON (entry.value ->> 'platform')
           entry.value ->> 'platform' AS platform,
           entry.value,
           entry.ordinality AS sort_order
    FROM channel_monitor_v2_config config
    CROSS JOIN LATERAL jsonb_array_elements(config.platforms) WITH ORDINALITY AS entry(value, ordinality)
    WHERE jsonb_typeof(entry.value) = 'object'
      AND entry.value ->> 'platform' IN (SELECT platform FROM catalog)
    ORDER BY entry.value ->> 'platform', entry.ordinality
),
normalized AS (
    SELECT config.id,
           COALESCE(
               jsonb_agg(items.value ORDER BY items.phase, items.sort_order),
               '[]'::jsonb
           ) AS platforms
    FROM channel_monitor_v2_config config
    LEFT JOIN (
        SELECT value, 0 AS phase, sort_order
        FROM existing
        UNION ALL
        SELECT jsonb_build_object('platform', catalog.platform, 'enabled', TRUE, 'models', '[]'::jsonb),
               1 AS phase,
               catalog.sort_order
        FROM catalog
        LEFT JOIN existing ON existing.platform = catalog.platform
        WHERE existing.platform IS NULL
    ) items ON TRUE
    GROUP BY config.id
)
UPDATE channel_monitor_v2_config config
SET platforms = normalized.platforms,
    version = config.version + 1,
    updated_at = NOW()
FROM normalized
WHERE config.id = normalized.id
  AND config.platforms IS DISTINCT FROM normalized.platforms;

ALTER TABLE channel_monitor_v2_config
    ALTER COLUMN platforms SET DEFAULT '[{"platform":"anthropic","enabled":true,"models":[]},{"platform":"openai","enabled":true,"models":[]},{"platform":"gemini","enabled":true,"models":[]},{"platform":"antigravity","enabled":true,"models":[]},{"platform":"grok","enabled":true,"models":[]},{"platform":"kimi","enabled":true,"models":[]},{"platform":"zhipu","enabled":true,"models":[]},{"platform":"deepseek","enabled":true,"models":[]},{"platform":"minimax","enabled":true,"models":[]},{"platform":"opencode_go","enabled":true,"models":[]},{"platform":"composite","enabled":true,"models":[]}]'::jsonb;

CREATE OR REPLACE FUNCTION channel_monitor_v2_cleanup_deleted_group()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $function$
DECLARE
    deleted_group_id BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        deleted_group_id := OLD.id;
    ELSE
        deleted_group_id := NEW.id;
    END IF;

    UPDATE channel_monitor_v2_config
    SET group_ids = array_remove(group_ids, deleted_group_id),
        version = version + 1,
        updated_at = NOW(),
        updated_by = NULL
    WHERE id = 1
      AND group_scope_mode = 'selected'
      AND deleted_group_id = ANY(group_ids);

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END
$function$;

DROP TRIGGER IF EXISTS channel_monitor_v2_cleanup_group_soft_delete ON groups;
CREATE TRIGGER channel_monitor_v2_cleanup_group_soft_delete
AFTER UPDATE OF deleted_at ON groups
FOR EACH ROW
WHEN (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL)
EXECUTE FUNCTION channel_monitor_v2_cleanup_deleted_group();

DROP TRIGGER IF EXISTS channel_monitor_v2_cleanup_group_delete ON groups;
CREATE TRIGGER channel_monitor_v2_cleanup_group_delete
AFTER DELETE ON groups
FOR EACH ROW
EXECUTE FUNCTION channel_monitor_v2_cleanup_deleted_group();
