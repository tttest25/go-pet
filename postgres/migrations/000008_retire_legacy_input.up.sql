CREATE OR REPLACE FUNCTION public.sm_create_action(
    p_action text,
    p_version integer,
    p_data jsonb
)
RETURNS TABLE (input_id uuid, process_id uuid, status text)
LANGUAGE plpgsql
AS $$
DECLARE
    v_payload jsonb;
    v_input_id uuid;
    v_process_id uuid;
    v_status text;
BEGIN
    IF p_version IS DISTINCT FROM 1 THEN
        RAISE EXCEPTION 'unsupported action version' USING ERRCODE = '22023';
    END IF;
    IF jsonb_typeof(p_data) IS DISTINCT FROM 'object' OR p_data ? 'tool' THEN
        RAISE EXCEPTION 'action data must be an object without a tool key' USING ERRCODE = '22023';
    END IF;

    v_payload := p_data || jsonb_build_object('tool', p_action);
    PERFORM public.sm_validate_tool_payload(v_payload);
    IF NOT EXISTS (
        SELECT FROM public.sm_actions
        WHERE action = (v_payload->>'tool') AND version = 1 AND enabled
    ) THEN
        RAISE EXCEPTION 'action % version 1 is unavailable', v_payload->>'tool'
            USING ERRCODE = '22023';
    END IF;

    INSERT INTO public.sm_inputs (source, payload)
    VALUES ('api', v_payload)
    RETURNING sm_inputs.id, sm_inputs.status INTO v_input_id, v_status;

    SELECT p.id INTO v_process_id
    FROM public.sm_processes AS p
    WHERE p.input_id = v_input_id
    ORDER BY p.created_at ASC
    LIMIT 1;

    IF v_process_id IS NULL THEN
        RAISE EXCEPTION 'process not created for input %', v_input_id
            USING ERRCODE = 'P0002';
    END IF;

    RETURN QUERY SELECT v_input_id, v_process_id, v_status;
END;
$$;

DROP FUNCTION public.sm_create_input(text, jsonb);
