CREATE TABLE public.sm_actions (
    action text COLLATE "C",
    version integer CHECK (version > 0),
    title text NOT NULL CHECK (btrim(title) <> ''),
    enabled boolean NOT NULL,
    PRIMARY KEY (action, version)
);

INSERT INTO public.sm_actions (action, version, title, enabled)
VALUES
    ('echo-test', 1, 'Echo test', true),
    ('fabric', 1, 'Fabric', true),
    ('yt-dlp', 1, 'yt-dlp', true);

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

    IF NOT EXISTS (
        SELECT FROM public.sm_actions
        WHERE action = (p_payload->>'tool') AND version = 1 AND enabled
    ) THEN
        RAISE EXCEPTION 'action % version 1 is unavailable', p_payload->>'tool'
            USING ERRCODE = '22023';
    END IF;

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
