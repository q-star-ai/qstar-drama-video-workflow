BEGIN;
DROP VIEW IF EXISTS video_job_attempts;
DROP TABLE IF EXISTS gpu_instance_leases;
DROP TABLE IF EXISTS video_jobs;
COMMIT;
