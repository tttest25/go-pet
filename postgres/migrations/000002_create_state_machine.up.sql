CREATE TABLE sm_inputs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    source TEXT NOT NULL,
    payload JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'new',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT sm_inputs_status_check
        CHECK (status IN ('new', 'processed', 'error'))
);

CREATE TABLE sm_processes (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    input_id UUID NOT NULL REFERENCES sm_inputs(id) ON DELETE CASCADE,
    state TEXT NOT NULL DEFAULT 'init',
    context JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT sm_processes_state_check
        CHECK (state IN ('init', 'running', 'completed', 'failed'))
);

CREATE TABLE sm_jobs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    process_id UUID NOT NULL REFERENCES sm_processes(id) ON DELETE CASCADE,
    tool_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    status TEXT NOT NULL DEFAULT 'pending',
    priority INT NOT NULL DEFAULT 0,
    result_path TEXT,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT sm_jobs_tool_name_check
        CHECK (tool_name IN ('yt-dlp', 'fabric', 'echo-test')),

    CONSTRAINT sm_jobs_status_check
        CHECK (
            status IN (
                'pending',
                'dispatched',
                'in_progress',
                'completed',
                'failed'
            )
        )
);

CREATE TABLE sm_job_logs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES sm_jobs(id) ON DELETE CASCADE,
    old_status TEXT,
    new_status TEXT NOT NULL,
    message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE sm_outputs (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    process_id UUID NOT NULL REFERENCES sm_processes(id) ON DELETE CASCADE,
    artifact_type TEXT NOT NULL,
    content TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Индексы внешних ключей для связей и каскадного удаления.
CREATE INDEX sm_processes_input_id_idx
    ON sm_processes (input_id);

CREATE INDEX sm_jobs_process_id_idx
    ON sm_jobs (process_id);

CREATE INDEX sm_job_logs_job_id_created_at_idx
    ON sm_job_logs (job_id, created_at);

CREATE INDEX sm_outputs_process_id_idx
    ON sm_outputs (process_id);

-- Поддерживает PickAndDispatchJob:
-- WHERE status = 'pending'
-- ORDER BY priority DESC, created_at ASC.
CREATE INDEX sm_jobs_pending_pick_idx
    ON sm_jobs (priority DESC, created_at ASC)
    WHERE status = 'pending';

/*
 * Создание процесса и первой задачи из входящего события.
 * Ошибка, например отсутствующий или неподдерживаемый payload->>'tool',
 * откатывает всю цепочку вместе с исходной записью sm_inputs.
 */
CREATE FUNCTION sm_inputs_after_insert_fn()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    v_process_id UUID;
BEGIN
    INSERT INTO sm_processes (input_id)
    VALUES (NEW.id)
    RETURNING id INTO v_process_id;

    INSERT INTO sm_jobs (
        process_id,
        tool_name,
        payload
    )
    VALUES (
        v_process_id,
        NEW.payload->>'tool',
        NEW.payload
    );

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_sm_inputs_after_insert
AFTER INSERT ON sm_inputs
FOR EACH ROW
EXECUTE FUNCTION sm_inputs_after_insert_fn();

/*
 * Уведомление отправляется только при появлении pending-задачи.
 * pg_notify публикует уведомление после успешного COMMIT,
 * поэтому откаченная цепочка не разбудит executor.
 */
CREATE FUNCTION sm_jobs_notify_fn()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.status = 'pending' THEN
        IF TG_OP = 'INSERT' THEN
            PERFORM pg_notify('sm_jobs_channel', NEW.id::text);
        ELSIF OLD.status IS DISTINCT FROM NEW.status THEN
            PERFORM pg_notify('sm_jobs_channel', NEW.id::text);
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_sm_jobs_notify
AFTER INSERT OR UPDATE OF status ON sm_jobs
FOR EACH ROW
EXECUTE FUNCTION sm_jobs_notify_fn();

/*
 * Аудит фактического перехода статуса и синхронизация процесса.
 * Для MVP процесс управляется одной линейной задачей.
 */
CREATE FUNCTION sm_jobs_status_audit_fn()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO sm_job_logs (
        job_id,
        old_status,
        new_status,
        message
    )
    VALUES (
        NEW.id,
        OLD.status,
        NEW.status,
        NEW.error_msg
    );

    UPDATE sm_processes
    SET
        state = CASE NEW.status
            WHEN 'pending' THEN 'init'
            WHEN 'dispatched' THEN 'running'
            WHEN 'in_progress' THEN 'running'
            WHEN 'completed' THEN 'completed'
            WHEN 'failed' THEN 'failed'
        END,
        updated_at = NOW()
    WHERE id = NEW.process_id;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_sm_jobs_status_audit
AFTER UPDATE OF status ON sm_jobs
FOR EACH ROW
WHEN (OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION sm_jobs_status_audit_fn();
