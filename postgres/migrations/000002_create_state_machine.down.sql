DROP TABLE IF EXISTS sm_outputs;
DROP TABLE IF EXISTS sm_job_logs;
DROP TABLE IF EXISTS sm_jobs;
DROP TABLE IF EXISTS sm_processes;
DROP TABLE IF EXISTS sm_inputs;

DROP FUNCTION IF EXISTS sm_jobs_status_audit_fn();
DROP FUNCTION IF EXISTS sm_jobs_notify_fn();
DROP FUNCTION IF EXISTS sm_inputs_after_insert_fn();
