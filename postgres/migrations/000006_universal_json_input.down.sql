DROP FUNCTION IF EXISTS public.sm_create_action(text, integer, jsonb);
DROP FUNCTION IF EXISTS public.sm_pick_next_job_with_version();
ALTER TABLE IF EXISTS public.sm_jobs DROP COLUMN IF EXISTS action_version;
