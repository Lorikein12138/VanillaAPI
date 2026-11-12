-- Preserve the full parent billing ID plus the advisor iteration suffix.
ALTER TABLE usage_logs ALTER COLUMN request_id TYPE VARCHAR(255);
