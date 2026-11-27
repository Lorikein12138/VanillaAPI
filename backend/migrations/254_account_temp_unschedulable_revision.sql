ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS temp_unschedulable_revision BIGINT NOT NULL DEFAULT 0;
