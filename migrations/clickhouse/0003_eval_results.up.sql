CREATE TABLE IF NOT EXISTS eval_results (
    run_id              String,
    request_id          String,
    ts                  DateTime64(3),
    baseline_model      String,
    candidate_model     String,
    baseline_cost_usd   Float64,
    candidate_cost_usd  Float64,
    score               Float64
)
ENGINE = MergeTree()
ORDER BY (ts, run_id)
