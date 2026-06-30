CREATE TABLE IF NOT EXISTS request_logs (
    ts                 DateTime64(3),
    request_id         String,
    key_id             String,
    team_id            String,
    requested_model    String,
    served_model       String,
    provider           String,
    input_tokens       UInt32,
    output_tokens      UInt32,
    cache_read_tokens  UInt32,
    cache_write_tokens UInt32,
    cost_usd           Float64,
    latency_ms         UInt32,
    cache_hit          UInt8,
    status             UInt16
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(ts)
ORDER BY (ts, team_id)
