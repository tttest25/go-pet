ALTER TABLE public.sm_jobs
    ADD COLUMN action_version integer NOT NULL DEFAULT 1
    CONSTRAINT sm_jobs_action_version_positive CHECK (action_version > 0);

CREATE FUNCTION public.sm_create_action(
    p_action text,
    p_version integer,
    p_data jsonb
)
RETURNS TABLE (input_id uuid, process_id uuid, status text)
LANGUAGE plpgsql
AS $$
BEGIN
    IF p_version IS DISTINCT FROM 1 THEN
        RAISE EXCEPTION 'unsupported action version' USING ERRCODE = '22023';
    END IF;
    IF jsonb_typeof(p_data) IS DISTINCT FROM 'object' OR p_data ? 'tool' THEN
        RAISE EXCEPTION 'action data must be an object without a tool key' USING ERRCODE = '22023';
    END IF;

    -- v1 uses the existing validator, active catalog check and enqueue trigger.
    RETURN QUERY
    SELECT c.input_id, c.process_id, c.status
    FROM public.sm_create_input('api', p_data || jsonb_build_object('tool', p_action)) AS c;
END;
$$;

CREATE FUNCTION public.sm_pick_next_job_with_version()
RETURNS TABLE (
    id uuid, process_id uuid, tool_name text, payload jsonb,
    old_status text, status text, action_version integer
)
LANGUAGE plpgsql
AS $$
DECLARE
    picked record;
BEGIN
    SELECT * INTO picked FROM public.sm_pick_next_job();
    IF NOT FOUND THEN
        RETURN;
    END IF;

    -- Dispatch can see a row committed after the caller took its snapshot.
    -- Read the locked row in a separate statement here, not in a caller join.
    SELECT j.action_version INTO STRICT action_version
    FROM public.sm_jobs AS j WHERE j.id = picked.id;
    RETURN QUERY SELECT picked.id, picked.process_id, picked.tool_name, picked.payload,
                        picked.old_status, picked.status, action_version;
END;
$$;
