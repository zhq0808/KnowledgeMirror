-- ============================================================================
-- 000012 · 费曼音频任务可恢复状态机
-- 先持久化任务，再调用 STT；状态变化进入 Append-only 事件表。
-- ============================================================================

DROP TRIGGER IF EXISTS trg_feynman_audio_tasks_append_only ON feynman_audio_tasks;
DROP FUNCTION IF EXISTS reject_feynman_audio_task_mutation();

ALTER TABLE feynman_audio_tasks
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE TABLE feynman_audio_task_events (
    event_id        BIGSERIAL    PRIMARY KEY,
    audio_task_id   UUID         NOT NULL,
    attempt_id      UUID         NOT NULL,
    user_id         VARCHAR(64)  NOT NULL,
    from_status     VARCHAR(20),
    to_status       VARCHAR(20)  NOT NULL,
    stt_provider    VARCHAR(50),
    stt_model       VARCHAR(100),
    stt_request_id  VARCHAR(128),
    error_message   TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT ck_feynman_audio_task_events_status CHECK (
        to_status IN ('uploaded', 'transcribing', 'transcribed', 'failed')
        AND (from_status IS NULL OR from_status IN ('uploaded', 'transcribing', 'transcribed', 'failed'))
    ),
    CONSTRAINT fk_feynman_audio_task_events_task
        FOREIGN KEY (audio_task_id, attempt_id, user_id)
        REFERENCES feynman_audio_tasks(audio_task_id, attempt_id, user_id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_feynman_audio_task_events_task
    ON feynman_audio_task_events (audio_task_id, event_id);

CREATE FUNCTION guard_feynman_audio_task_transition() RETURNS trigger AS $$
BEGIN
    IF NEW.audio_task_id IS DISTINCT FROM OLD.audio_task_id
        OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id
        OR NEW.user_id IS DISTINCT FROM OLD.user_id
        OR NEW.attempt_no IS DISTINCT FROM OLD.attempt_no
        OR NEW.mime_type IS DISTINCT FROM OLD.mime_type
        OR NEW.size_bytes IS DISTINCT FROM OLD.size_bytes
        OR NEW.duration_ms IS DISTINCT FROM OLD.duration_ms
        OR NEW.sha256 IS DISTINCT FROM OLD.sha256
        OR NEW.audio_data IS DISTINCT FROM OLD.audio_data
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'feynman audio task lineage is immutable'
            USING ERRCODE = '55000';
    END IF;

    IF OLD.status = 'transcribed' THEN
        RAISE EXCEPTION 'transcribed feynman audio task is immutable'
            USING ERRCODE = '55000';
    END IF;

    IF NOT (
        (OLD.status = 'uploaded' AND NEW.status IN ('transcribing', 'failed'))
        OR (OLD.status = 'transcribing' AND NEW.status IN ('transcribed', 'failed'))
        OR (OLD.status = 'failed' AND NEW.status = 'transcribing')
    ) THEN
        RAISE EXCEPTION 'invalid feynman audio task transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = '55000';
    END IF;

    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_feynman_audio_tasks_transition_guard
    BEFORE UPDATE ON feynman_audio_tasks
    FOR EACH ROW EXECUTE FUNCTION guard_feynman_audio_task_transition();

CREATE FUNCTION record_feynman_audio_task_event() RETURNS trigger AS $$
BEGIN
    INSERT INTO feynman_audio_task_events (
        audio_task_id, attempt_id, user_id, from_status, to_status,
        stt_provider, stt_model, stt_request_id, error_message
    ) VALUES (
        NEW.audio_task_id, NEW.attempt_id, NEW.user_id,
        CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE OLD.status END,
        NEW.status, NEW.stt_provider, NEW.stt_model, NEW.stt_request_id, NEW.transcript_error
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_feynman_audio_tasks_record_event
    AFTER INSERT OR UPDATE OF status ON feynman_audio_tasks
    FOR EACH ROW EXECUTE FUNCTION record_feynman_audio_task_event();

CREATE FUNCTION reject_feynman_audio_task_delete() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'feynman_audio_tasks cannot be deleted'
        USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_feynman_audio_tasks_no_delete
    BEFORE DELETE ON feynman_audio_tasks
    FOR EACH ROW EXECUTE FUNCTION reject_feynman_audio_task_delete();

INSERT INTO feynman_audio_task_events (
    audio_task_id, attempt_id, user_id, from_status, to_status,
    stt_provider, stt_model, stt_request_id, error_message, created_at
)
SELECT audio_task_id, attempt_id, user_id, NULL, status,
       stt_provider, stt_model, stt_request_id, transcript_error, created_at
FROM feynman_audio_tasks;

COMMENT ON TABLE feynman_audio_task_events IS '音频任务状态变化审计；Append-only';