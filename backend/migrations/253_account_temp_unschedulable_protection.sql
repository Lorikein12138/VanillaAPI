ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS temp_unschedulable_protected_until TIMESTAMPTZ;
