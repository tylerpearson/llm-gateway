ALTER TABLE request_logs
    DROP COLUMN IF EXISTS user_agent,
    DROP COLUMN IF EXISTS end_user,
    DROP COLUMN IF EXISTS tags
