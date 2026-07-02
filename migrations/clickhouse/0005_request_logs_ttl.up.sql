ALTER TABLE request_logs MODIFY TTL toDateTime(ts) + INTERVAL 90 DAY
