-- Drop in reverse FK order. experiment_metrics references experiments.
DROP TABLE IF EXISTS platform.experiment_metrics;
DROP TABLE IF EXISTS platform.experiments;
-- Leave the `platform` schema in place; other tables may live there.
