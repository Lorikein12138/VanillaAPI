-- Only interpret a recognizable plan. Leave legacy, invalid and unknown
-- payloads for an explicit repair. Use the same writer lock as the coordinator.
DO $$
DECLARE
    raw_plan text;
    raw_snapshot text;
    raw_lease text;
    plan jsonb;
    lease jsonb;
    snapshot jsonb;
    original jsonb;
    old_revision jsonb;
    kept_groups jsonb;
    kept_group_ids text[];
    field text;
    orphan_keys text[];
BEGIN
    INSERT INTO settings (key, value, updated_at)
    VALUES ('channel_detection_lease', '', clock_timestamp())
    ON CONFLICT (key) DO NOTHING;
    PERFORM 1 FROM settings WHERE key = 'channel_detection_lease' FOR UPDATE;

    -- Active associations only. API keys, billing and audit history are retained.
    DELETE FROM channel_groups AS link
    USING groups AS g
    WHERE link.group_id = g.id AND g.deleted_at IS NOT NULL;

    DELETE FROM user_group_rate_multipliers AS rate
    USING groups AS g
    WHERE rate.group_id = g.id AND g.deleted_at IS NOT NULL;

    SELECT value INTO raw_plan FROM settings WHERE key = 'channel_detection_config' FOR SHARE;
    BEGIN
        plan := raw_plan::jsonb;
    EXCEPTION WHEN data_exception THEN
        RETURN;
    END;
    IF jsonb_typeof(plan->'groups') IS DISTINCT FROM 'array' OR
       jsonb_typeof(plan->'revision') IS DISTINCT FROM 'string' THEN
        RETURN;
    END IF;
    IF EXISTS (
        SELECT 1 FROM jsonb_array_elements(plan->'groups') AS choice
        WHERE jsonb_typeof(choice->'group_id') IS DISTINCT FROM 'number'
           OR (choice->>'group_id') !~ '^[1-9][0-9]*$'
    ) THEN
        RETURN;
    END IF;

    old_revision := plan->'revision';
    SELECT COALESCE(jsonb_agg(choice ORDER BY position) FILTER (WHERE g.id IS NOT NULL), '[]')
    INTO kept_groups
    FROM jsonb_array_elements(plan->'groups') WITH ORDINALITY AS choices(choice, position)
    LEFT JOIN groups AS g ON g.id::text = choice->>'group_id' AND g.deleted_at IS NULL;
    IF kept_groups IS DISTINCT FROM plan->'groups' THEN
        plan := jsonb_set(plan, '{groups}', kept_groups);
        plan := jsonb_set(plan, '{revision}', to_jsonb(gen_random_uuid()::text));
        IF kept_groups = '[]'::jsonb THEN
            plan := jsonb_set(plan, '{enabled}', 'false');
        END IF;
        UPDATE settings SET value = plan::text, updated_at = clock_timestamp()
        WHERE key = 'channel_detection_config';

    END IF;
    kept_group_ids := ARRAY(SELECT choice->>'group_id' FROM jsonb_array_elements(kept_groups) AS choice);

    SELECT value INTO raw_lease FROM settings WHERE key = 'channel_detection_lease';
    BEGIN
        lease := raw_lease::jsonb;
    EXCEPTION WHEN data_exception THEN
        lease := NULL; -- Released, legacy UUID-only or an unknown future format.
    END;
    IF lease->'v' = '2'::jsonb AND jsonb_typeof(lease->'groups') = 'object' THEN
        SELECT array_agg(id) INTO orphan_keys
        FROM jsonb_object_keys(lease->'groups') AS keys(id)
        WHERE id ~ '^[1-9][0-9]*$' AND id <> ALL(kept_group_ids);
        IF orphan_keys IS NOT NULL THEN
            lease := jsonb_set(lease, '{groups}', (lease->'groups') - orphan_keys);
            UPDATE settings
            SET value = CASE WHEN lease->'groups' = '{}'::jsonb THEN '' ELSE lease::text END,
                updated_at = CASE WHEN lease->'groups' = '{}'::jsonb THEN clock_timestamp() ELSE updated_at END
            WHERE key = 'channel_detection_lease';
        END IF;
    END IF;

    DELETE FROM settings AS rpm
    WHERE rpm.key ~ '^channel_detection_group_rpm:[1-9][0-9]*$'
      AND split_part(rpm.key, ':', 2) <> ALL(kept_group_ids);

    SELECT value INTO raw_snapshot FROM settings WHERE key = 'channel_detection_latest' FOR UPDATE;
    BEGIN
        snapshot := raw_snapshot::jsonb;
    EXCEPTION WHEN data_exception THEN
        RETURN;
    END;
    IF jsonb_typeof(snapshot) IS DISTINCT FROM 'object' OR
       snapshot->'revision' IS DISTINCT FROM old_revision THEN
        RETURN;
    END IF;
    original := snapshot;
    snapshot := jsonb_set(snapshot, '{revision}', plan->'revision');
    IF jsonb_typeof(snapshot->'running'->'group_id') = 'number'
       AND (snapshot->'running'->>'group_id') ~ '^[1-9][0-9]*$'
       AND (snapshot->'running'->>'group_id') <> ALL(kept_group_ids) THEN
        snapshot := snapshot - 'running';
    END IF;
    FOREACH field IN ARRAY ARRAY['samples', 'attempted_at', 'running_by_group', 'proofs', 'no_send']
    LOOP
        IF jsonb_typeof(snapshot->field) IS DISTINCT FROM 'object' THEN
            CONTINUE;
        END IF;
        SELECT array_agg(id) INTO orphan_keys
        FROM jsonb_object_keys(snapshot->field) AS keys(id)
        WHERE id ~ '^[1-9][0-9]*$' AND id <> ALL(kept_group_ids);
        IF orphan_keys IS NOT NULL THEN
            snapshot := jsonb_set(snapshot, ARRAY[field], (snapshot->field) - orphan_keys);
        END IF;
    END LOOP;
    IF snapshot IS DISTINCT FROM original THEN
        UPDATE settings SET value = snapshot::text, updated_at = clock_timestamp()
        WHERE key = 'channel_detection_latest';
    END IF;
END $$;
