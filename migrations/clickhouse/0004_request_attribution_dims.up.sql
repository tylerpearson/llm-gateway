ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS user_agent String,
    ADD COLUMN IF NOT EXISTS end_user String,
    ADD COLUMN IF NOT EXISTS tags Array(String)
