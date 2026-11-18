CREATE OR REPLACE FUNCTION public.sm_create_input(
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
    PERFORM public.sm_validate_tool_payload(p_payload);

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

DROP TABLE IF EXISTS public.sm_actions;
