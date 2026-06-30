CREATE TABLE IF NOT EXISTS eval_runs (
    id              String,
    created_at      DateTime64(3),
    name            String,
    baseline_model  String,
    candidate_model String,
    status          String
)
ENGINE = MergeTree()
ORDER BY (created_at, id)
