-- Extend passive V2 monitoring to the complete v0.2.15 group platform catalog.
-- Preserve each first supported object and its order; append missing defaults.
-- The migration runner records this file and its checksum in one transaction.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

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
        ('typesafe', 11),
        ('command_code', 12),
        ('cline', 13),
        ('composite', 14)
), existing AS (
    SELECT DISTINCT ON (config.id, entry.value ->> 'platform')
           config.id AS config_id,
           entry.value ->> 'platform' AS platform,
           entry.value,
           entry.ordinality AS sort_order
    FROM channel_monitor_v2_config config
    CROSS JOIN LATERAL jsonb_array_elements(config.platforms) WITH ORDINALITY AS entry(value, ordinality)
    WHERE jsonb_typeof(entry.value) = 'object'
      AND entry.value ->> 'platform' IN (SELECT platform FROM catalog)
    ORDER BY config.id, entry.value ->> 'platform', entry.ordinality
), normalized AS (
    SELECT config.id,
           COALESCE(
               jsonb_agg(items.value ORDER BY items.phase, items.sort_order),
               '[]'::jsonb
           ) AS platforms
    FROM channel_monitor_v2_config config
    LEFT JOIN (
        SELECT config_id, value, 0 AS phase, sort_order
        FROM existing
        UNION ALL
        SELECT config.id,
               jsonb_build_object('platform', catalog.platform, 'enabled', TRUE, 'models', '[]'::jsonb),
               1 AS phase,
               catalog.sort_order
        FROM channel_monitor_v2_config config
        CROSS JOIN catalog
        LEFT JOIN existing ON existing.config_id = config.id AND existing.platform = catalog.platform
        WHERE existing.platform IS NULL
    ) items ON items.config_id = config.id
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
    ALTER COLUMN platforms SET DEFAULT '[{"platform":"anthropic","enabled":true,"models":[]},{"platform":"openai","enabled":true,"models":[]},{"platform":"gemini","enabled":true,"models":[]},{"platform":"antigravity","enabled":true,"models":[]},{"platform":"grok","enabled":true,"models":[]},{"platform":"kimi","enabled":true,"models":[]},{"platform":"zhipu","enabled":true,"models":[]},{"platform":"deepseek","enabled":true,"models":[]},{"platform":"minimax","enabled":true,"models":[]},{"platform":"opencode_go","enabled":true,"models":[]},{"platform":"typesafe","enabled":true,"models":[]},{"platform":"command_code","enabled":true,"models":[]},{"platform":"cline","enabled":true,"models":[]},{"platform":"composite","enabled":true,"models":[]}]'::jsonb;
