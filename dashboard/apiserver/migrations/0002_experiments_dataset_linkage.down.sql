DROP VIEW IF EXISTS platform.experiment_lineage;

ALTER TABLE platform.experiments
    DROP COLUMN IF EXISTS dataset_version_id,
    DROP COLUMN IF EXISTS split,
    DROP COLUMN IF EXISTS schedule_id,
    DROP COLUMN IF EXISTS workflow_id;

DROP INDEX IF EXISTS platform.idx_experiments_dataset_version_id;
DROP INDEX IF EXISTS platform.idx_experiments_schedule_id;
DROP INDEX IF EXISTS platform.idx_experiments_workflow_id;
