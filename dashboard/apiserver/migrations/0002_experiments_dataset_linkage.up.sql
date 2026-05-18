-- Bind experiments to evo_datasets.dataset_versions so the experiments
-- page can show what data a run trained on, and the new "Launch from
-- dataset" flow can pre-fill the dataset+split. schedule_id + workflow_id
-- are for the Temporal-driven flows that follow (recurring sweeps + a live
-- training run respectively).
--
-- evo_datasets is owned by the dataset-manager (evolve/dataset-manager/store/migrations).
-- We don't recreate it here; we only reference it. ON DELETE SET NULL so
-- archiving a dataset version doesn't cascade-wipe experiment history.

ALTER TABLE platform.experiments
    ADD COLUMN IF NOT EXISTS dataset_version_id UUID
        REFERENCES evo_datasets.dataset_versions(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS split       TEXT,
    ADD COLUMN IF NOT EXISTS schedule_id UUID,
    ADD COLUMN IF NOT EXISTS workflow_id TEXT;

CREATE INDEX IF NOT EXISTS idx_experiments_dataset_version_id
    ON platform.experiments (dataset_version_id);
CREATE INDEX IF NOT EXISTS idx_experiments_schedule_id
    ON platform.experiments (schedule_id)
    WHERE schedule_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_experiments_workflow_id
    ON platform.experiments (workflow_id)
    WHERE workflow_id IS NOT NULL;

-- experiment_lineage: flatten the parent_id self-reference into a tree
-- the frontend LineageTree can render with a single GET. root_id is the
-- top ancestor (NULL parent); depth is 0 at the root. The CTE is bounded
-- by the table size - parent_id has a FK so cycles are impossible.
CREATE OR REPLACE VIEW platform.experiment_lineage AS
WITH RECURSIVE tree AS (
    SELECT
        id,
        parent_id,
        id    AS root_id,
        0     AS depth,
        ARRAY[id]::uuid[] AS path
    FROM platform.experiments
    WHERE parent_id IS NULL

    UNION ALL

    SELECT
        e.id,
        e.parent_id,
        t.root_id,
        t.depth + 1,
        t.path || e.id
    FROM platform.experiments e
    JOIN tree t ON e.parent_id = t.id
)
SELECT
    t.id,
    t.parent_id,
    t.root_id,
    t.depth,
    t.path,
    e.name,
    e.status,
    e.val_bpb,
    e.started_at,
    e.created_at
FROM tree t
JOIN platform.experiments e ON e.id = t.id;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'postgrest_anon') THEN
        EXECUTE 'GRANT SELECT ON platform.experiment_lineage TO postgrest_anon';
    END IF;
END$$;
