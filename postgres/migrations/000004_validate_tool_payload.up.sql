CREATE OR REPLACE FUNCTION public.sm_validate_tool_payload(p_payload JSONB)
RETURNS VOID
LANGUAGE plpgsql
AS $validator$
DECLARE
    v_tool TEXT COLLATE "C";
    v_url TEXT;
    v_authority TEXT;
    v_ipv6 TEXT;
BEGIN
    IF jsonb_typeof(p_payload) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'payload must be an object' USING ERRCODE = '22023';
    END IF;
    IF jsonb_typeof(p_payload->'tool') IS DISTINCT FROM 'string' THEN
        RAISE EXCEPTION 'payload.tool must be a string' USING ERRCODE = '22023';
    END IF;

    v_tool := p_payload->>'tool';
    CASE v_tool
        WHEN 'echo-test' THEN
            IF p_payload ? 'message' AND (
                jsonb_typeof(p_payload->'message') IS DISTINCT FROM 'string'
                OR (p_payload->>'message') !~ '[^[:space:]]'
            ) THEN
                RAISE EXCEPTION 'payload.message must be a nonblank string' USING ERRCODE = '22023';
            END IF;
        WHEN 'fabric' THEN
            IF jsonb_typeof(p_payload->'text') IS DISTINCT FROM 'string'
                OR (p_payload->>'text') !~ '[^[:space:]]' THEN
                RAISE EXCEPTION 'payload.text must be a nonblank string' USING ERRCODE = '22023';
            END IF;
            IF p_payload ? 'pattern' AND (
                jsonb_typeof(p_payload->'pattern') IS DISTINCT FROM 'string'
                OR (p_payload->>'pattern') COLLATE "C" !~ '^[A-Za-z0-9_-]{1,128}$'
            ) THEN
                RAISE EXCEPTION 'payload.pattern must be a simple name' USING ERRCODE = '22023';
            END IF;
        WHEN 'yt-dlp' THEN
            IF jsonb_typeof(p_payload->'url') IS DISTINCT FROM 'string' THEN
                RAISE EXCEPTION 'payload.url must be a string' USING ERRCODE = '22023';
            END IF;
            v_url := p_payload->>'url';
            v_authority := substring(v_url COLLATE "C" FROM '(?i)^https?://([^/?#]*)');
            IF v_authority IS NULL OR v_url ~ '[[:space:][:cntrl:]]'
                OR strpos(v_url, chr(92)) <> 0
                OR v_url COLLATE "C" ~ '%(?![0-9A-Fa-f]{2})'
                OR v_authority COLLATE "C" !~ $authority$^([A-Za-z0-9._~%!$&'()*+,;=:-]*@)?([A-Za-z0-9._~%!$&'()*+,;=-]+|\[[^]]+\])(:[0-9]*)?$$authority$ THEN
                RAISE EXCEPTION 'payload.url must be an absolute HTTP(S) URL with a host' USING ERRCODE = '22023';
            END IF;
            -- A bracketed host is a literal, not a DNS name. inet checks IPv6
            -- syntax without resolving it; an optional encoded zone is separate.
            IF strpos(v_authority, '[') <> 0 THEN
                v_ipv6 := substring(v_authority FROM '\[([^]]+)\]');
                IF v_ipv6 COLLATE "C" !~ '^[A-Fa-f0-9:.]+(%25([A-Za-z0-9._~-]|%[0-9A-Fa-f]{2})+)?$'
                    OR strpos(v_ipv6, ':') = 0
                    OR NOT pg_input_is_valid(split_part(v_ipv6, '%25', 1), 'inet') THEN
                    RAISE EXCEPTION 'payload.url contains an invalid IPv6 host' USING ERRCODE = '22023';
                END IF;
            END IF;
        ELSE
            RAISE EXCEPTION 'unsupported payload.tool' USING ERRCODE = '22023';
    END CASE;
END;
$validator$;

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
