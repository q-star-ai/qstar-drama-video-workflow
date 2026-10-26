-- Unified short-drama video jobs. Additive only: the site's existing
-- VideoGeneration rows stay as they are and link via legacy_video_generation_id
-- until the authoritative schema is confirmed and a backfill is reviewed.
BEGIN;

CREATE TABLE IF NOT EXISTS video_jobs (
    id                         text PRIMARY KEY,
    idempotency_key            text NOT NULL UNIQUE,
    revision                   integer NOT NULL DEFAULT 0,
    status                     text NOT NULL CHECK (status IN ('queued','running','needs_reconcile','probing','qc_pending','succeeded','failed','blocked','cancelled')),
    episode_id                 text NOT NULL,
    shot_id                    text NOT NULL,
    input_mode                 text NOT NULL CHECK (input_mode IN ('identity_refs','first_last','video_ref')),
    current_provider           text,
    current_model              text,
    current_external_task_id   text,
    total_cost_micro_usd       bigint NOT NULL DEFAULT 0,
    cost_cap_micro_usd         bigint NOT NULL DEFAULT 0,
    legacy_video_generation_id text,
    -- Full job: request with asset IDs/versions/sha256 (no signed URLs),
    -- candidate chain, attempts, fallbacks, ffprobe output and QC.
    doc                        jsonb NOT NULL,
    created_at                 timestamptz NOT NULL,
    updated_at                 timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS video_jobs_shot_idx ON video_jobs (episode_id, shot_id);
CREATE INDEX IF NOT EXISTS video_jobs_open_idx ON video_jobs (status) WHERE status IN ('queued','running','needs_reconcile','probing','qc_pending');
-- One external task (e.g. an H3 prompt_id) belongs to one job.
CREATE UNIQUE INDEX IF NOT EXISTS video_jobs_external_task_idx ON video_jobs (current_provider, current_external_task_id) WHERE current_external_task_id IS NOT NULL;

-- Which job started which GPU instance; only the owner may auto-stop it.
CREATE TABLE IF NOT EXISTS gpu_instance_leases (
    instance_id           text PRIMARY KEY,
    provider              text NOT NULL,
    owner_job_id          text NOT NULL REFERENCES video_jobs (id),
    hourly_rate_micro_usd bigint NOT NULL,
    started_at            timestamptz NOT NULL,
    stopped_at            timestamptz
);

-- Per-attempt audit for cost and failover reporting.
CREATE OR REPLACE VIEW video_job_attempts AS
SELECT j.id AS job_id, j.episode_id, j.shot_id, a.ordinality AS attempt_no,
       a.value->>'provider' AS provider, a.value->>'model' AS model, a.value->>'kind' AS kind,
       a.value->>'external_task_id' AS external_task_id, a.value->>'status' AS status,
       a.value->>'error_class' AS error_class, (a.value->>'cost_micro_usd')::bigint AS cost_micro_usd,
       (a.value->>'gpu_seconds')::double precision AS gpu_seconds, a.value->>'instance_id' AS instance_id,
       (a.value->>'started_at')::timestamptz AS started_at
FROM video_jobs j, jsonb_array_elements(COALESCE(NULLIF(j.doc->'attempts', 'null'::jsonb), '[]'::jsonb)) WITH ORDINALITY a;

COMMIT;
