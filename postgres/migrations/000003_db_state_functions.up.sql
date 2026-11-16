CREATE OR REPLACE FUNCTION sm_create_input(
    p_source TEXT,
    p_payload JSONB
)
RETURNS TABLE (
    input_id UUID,
    process_id UUID,
    status TEXT
)
LANGUAGE plpgsql
AS $$
DECLARE
    v_input_id UUID;
    v_process_id UUID;
    v_status TEXT;
BEGIN
    INSERT INTO sm_inputs (source, payload)
    VALUES (p_source, p_payload)
    RETURNING sm_inputs.id, sm_inputs.status
    INTO v_input_id, v_status;

    SELECT p.id
    INTO v_process_id
    FROM sm_processes AS p
    WHERE p.input_id = v_input_id
    ORDER BY p.created_at ASC
    LIMIT 1;

    IF v_process_id IS NULL THEN
        RAISE EXCEPTION 'process not created for input %', v_input_id
            USING ERRCODE = 'P0002';
    END IF;

    RETURN QUERY
    SELECT v_input_id, v_process_id, v_status;
END;
$$;

CREATE OR REPLACE FUNCTION sm_pick_next_job()
RETURNS TABLE (
    id UUID,
    process_id UUID,
    tool_name TEXT,
    payload JSONB,
    old_status TEXT,
    status TEXT
)
LANGUAGE plpgsql
AS $$
BEGIN
    RETURN QUERY
    UPDATE sm_jobs AS j
    SET
        status = 'dispatched',
        updated_at = NOW()
    WHERE j.id = (
        SELECT sub.id
        FROM sm_jobs AS sub
        WHERE sub.status = 'pending'
        ORDER BY sub.priority DESC, sub.created_at ASC
        FOR UPDATE SKIP LOCKED
        LIMIT 1
    )
    RETURNING
        j.id,
        j.process_id,
        j.tool_name,
        j.payload,
        OLD.status,
        NEW.status;
END;
$$;

CREATE OR REPLACE FUNCTION sm_update_job_status(
    p_job_id UUID,
    p_status TEXT,
    p_result_path TEXT DEFAULT NULL,
    p_error_msg TEXT DEFAULT NULL
)
RETURNS VOID
LANGUAGE plpgsql
AS $$
DECLARE
    v_process_id UUID;
BEGIN
    IF p_status IS NULL OR p_status NOT IN ('in_progress', 'completed', 'failed') THEN
        RAISE EXCEPTION 'invalid callback status % for job %', p_status, p_job_id
            USING ERRCODE = '22023';
    END IF;

    UPDATE sm_jobs AS j
    SET
        status = p_status,
        result_path = NULLIF(p_result_path, ''),
        error_msg = NULLIF(p_error_msg, ''),
        updated_at = NOW()
    WHERE j.id = p_job_id
      AND j.status IN ('dispatched', 'in_progress')
    RETURNING j.process_id
    INTO v_process_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'job % not found or cannot transition to %', p_job_id, p_status
            USING ERRCODE = 'P0002';
    END IF;

    IF p_status = 'completed' THEN
        INSERT INTO sm_outputs (
            process_id,
            artifact_type,
            metadata
        )
        VALUES (
            v_process_id,
            'result',
            jsonb_build_object('result_path', NULLIF(p_result_path, ''))
        );
    END IF;
END;
$$;
