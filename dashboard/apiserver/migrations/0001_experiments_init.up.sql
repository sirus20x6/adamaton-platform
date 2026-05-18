-- Codifies the schema that was originally created by the retired
-- adamaton-platform-backend FastAPI service (Alembic). The tables already
-- exist on pi5 with this exact shape; `IF NOT EXISTS` makes this a no-op
-- there while still being reproducible on fresh databases.
--
-- Schema mirrors `\d+ platform.experiments` from pi5 (2026-05-18):
--   id, agent_session_id, name, hypothesis, code_diff, val_bpb,
--   peak_memory_mb, status, notes, parent_id, commit_hash, tags,
--   started_at, finished_at, created_at, island, generation, evo_strategy.

CREATE SCHEMA IF NOT EXISTS platform;

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- for gen_random_uuid()

CREATE TABLE IF NOT EXISTS platform.experiments (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_session_id UUID,
    name             TEXT        NOT NULL,
    hypothesis       TEXT        NOT NULL,
    code_diff        TEXT,
    val_bpb          REAL,
    peak_memory_mb   INTEGER,
    status           TEXT        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','running','succeeded','failed','interrupted')),
    notes            TEXT,
    parent_id        UUID        REFERENCES platform.experiments(id) ON DELETE SET NULL,
    commit_hash      TEXT,
    tags             JSONB       DEFAULT '[]'::jsonb,
    started_at       TIMESTAMPTZ DEFAULT NOW(),
    finished_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ DEFAULT NOW(),
    island           SMALLINT    NOT NULL DEFAULT 0,
    generation       SMALLINT    NOT NULL DEFAULT 0,
    evo_strategy     TEXT
);

CREATE INDEX IF NOT EXISTS idx_experiments_agent_session_id
    ON platform.experiments (agent_session_id);
CREATE INDEX IF NOT EXISTS idx_experiments_parent_id
    ON platform.experiments (parent_id);
CREATE INDEX IF NOT EXISTS idx_experiments_started_at
    ON platform.experiments (started_at DESC);
CREATE INDEX IF NOT EXISTS idx_experiments_val_bpb
    ON platform.experiments (val_bpb);
CREATE INDEX IF NOT EXISTS ix_experiments_island_val_bpb
    ON platform.experiments (island, val_bpb);
CREATE INDEX IF NOT EXISTS idx_experiments_name_trgm
    ON platform.experiments USING gin (name gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_experiments_hypothesis_trgm
    ON platform.experiments USING gin (hypothesis gin_trgm_ops);

-- One row per metric sample. Bulk endpoint pivots ids x keys; per-experiment
-- endpoint slices on experiment_id. Step is monotonic per (experiment_id,key)
-- but the table doesn't enforce it - trainers occasionally re-emit.
CREATE TABLE IF NOT EXISTS platform.experiment_metrics (
    id            BIGSERIAL   PRIMARY KEY,
    experiment_id UUID        NOT NULL REFERENCES platform.experiments(id) ON DELETE CASCADE,
    step          INTEGER     NOT NULL,
    key           TEXT        NOT NULL,
    value         DOUBLE PRECISION NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_experiment_metrics_experiment_id
    ON platform.experiment_metrics (experiment_id);
CREATE INDEX IF NOT EXISTS idx_experiment_metrics_experiment_key_step
    ON platform.experiment_metrics (experiment_id, key, step);

-- PostgREST anon role read access. The Vite proxy hits /db/experiments and
-- /db/experiment_metrics on PostgREST, so the anon role must SELECT both.
-- Writes go through /platform/experiments (this apiserver), not PostgREST.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'postgrest_anon') THEN
        EXECUTE 'GRANT USAGE ON SCHEMA platform TO postgrest_anon';
        EXECUTE 'GRANT SELECT ON platform.experiments        TO postgrest_anon';
        EXECUTE 'GRANT SELECT ON platform.experiment_metrics TO postgrest_anon';
    END IF;
END$$;
