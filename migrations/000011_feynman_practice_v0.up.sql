-- ============================================================================
-- 000011 · 语音费曼练习 v0
-- 录音 -> STT 转写 -> 用户确认；以及知识点版本化 Rubric。
-- 本迁移不产生任何掌握状态或掌握证据：确认转写只是让文本进入下一阶段（费曼评估）的
-- 合法输入，Rubric 只定义评估看哪些维度，都不改变知识点的掌握等级。
-- ============================================================================

-- ---------------------------------------------------------------------------
-- 11.1 练习尝试：状态是派生值（见 confirmation_id / active_audio_task_id），
-- 不落单独 status 列，避免出现“派生状态”与“落库状态”不同步的问题。
-- ---------------------------------------------------------------------------
CREATE TABLE feynman_attempts (
    attempt_id           UUID         PRIMARY KEY,          -- 后端 UUIDv7
    user_id              VARCHAR(64)  NOT NULL,
    knowledge_point_id   UUID         NOT NULL,
    idempotency_key      VARCHAR(128) NOT NULL,
    active_audio_task_id UUID,                              -- 当前有效录音；确认前可被新录音替换
    confirmation_id      UUID,                              -- 一旦写入，该 attempt 永久只读

    created_by           VARCHAR(64)  NOT NULL,
    updated_by            VARCHAR(64)  NOT NULL,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_feynman_attempts_owner UNIQUE (attempt_id, user_id),
    CONSTRAINT uk_feynman_attempts_idempotency UNIQUE (user_id, idempotency_key),
    CONSTRAINT ck_feynman_attempts_idempotency_key CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT ck_feynman_attempts_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_feynman_attempts_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_feynman_attempts_user FOREIGN KEY (user_id)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_attempts_knowledge_point FOREIGN KEY (knowledge_point_id, user_id)
        REFERENCES knowledge_points(knowledge_point_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_attempts_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_attempts_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_feynman_attempts_user_updated
    ON feynman_attempts (user_id, updated_at DESC);
CREATE INDEX idx_feynman_attempts_knowledge_point
    ON feynman_attempts (knowledge_point_id, user_id);

CREATE TRIGGER trg_feynman_attempts_updated_at
    BEFORE UPDATE ON feynman_attempts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE feynman_attempts IS '一次语音费曼练习尝试；confirmation_id 写入后该行永久只读';
COMMENT ON COLUMN feynman_attempts.active_audio_task_id IS '当前有效录音；确认前允许重录替换，确认后锁死';
COMMENT ON COLUMN feynman_attempts.confirmation_id IS '指向转写确认记录；写入后触发器禁止再变更本行任何业务字段';

-- ---------------------------------------------------------------------------
-- 11.2 音频任务：Append-only。v0 同步调用 STT，写入时状态已是终态
-- （transcribed/failed）；status 枚举保留 uploaded/transcribing 供未来切换异步 Worker。
-- ---------------------------------------------------------------------------
CREATE TABLE feynman_audio_tasks (
    audio_task_id     UUID         PRIMARY KEY,          -- 后端 UUIDv7
    attempt_id        UUID         NOT NULL,
    user_id           VARCHAR(64)  NOT NULL,
    attempt_no        INTEGER      NOT NULL,             -- 同一 attempt 内第几次录音，从 1 开始

    status            VARCHAR(20)  NOT NULL,
    mime_type         VARCHAR(100) NOT NULL,
    size_bytes        INTEGER      NOT NULL,
    duration_ms       INTEGER,
    sha256            BYTEA        NOT NULL,
    audio_data        BYTEA        NOT NULL,

    stt_provider      VARCHAR(50),
    stt_model         VARCHAR(100),
    stt_request_id    VARCHAR(128),
    raw_transcript    TEXT,
    transcript_error  TEXT,

    created_by        VARCHAR(64)  NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_feynman_audio_tasks_owner UNIQUE (audio_task_id, attempt_id, user_id),
    CONSTRAINT uk_feynman_audio_tasks_attempt_no UNIQUE (attempt_id, attempt_no),
    -- 同一次尝试内重复上传完全相同的字节直接命中已有任务，不重复调用 STT。
    CONSTRAINT uk_feynman_audio_tasks_dedupe UNIQUE (attempt_id, sha256),
    CONSTRAINT ck_feynman_audio_tasks_attempt_no CHECK (attempt_no > 0),
    CONSTRAINT ck_feynman_audio_tasks_status CHECK (status IN (
        'uploaded', 'transcribing', 'transcribed', 'failed'
    )),
    CONSTRAINT ck_feynman_audio_tasks_mime CHECK (btrim(mime_type) <> ''),
    CONSTRAINT ck_feynman_audio_tasks_size CHECK (size_bytes > 0),
    CONSTRAINT ck_feynman_audio_tasks_duration CHECK (duration_ms IS NULL OR duration_ms > 0),
    CONSTRAINT ck_feynman_audio_tasks_sha256 CHECK (octet_length(sha256) = 32),
    CONSTRAINT ck_feynman_audio_tasks_terminal CHECK (
        (status = 'transcribed' AND raw_transcript IS NOT NULL AND transcript_error IS NULL)
        OR (status = 'failed' AND transcript_error IS NOT NULL AND raw_transcript IS NULL)
        OR (status IN ('uploaded', 'transcribing'))
    ),
    CONSTRAINT ck_feynman_audio_tasks_created_owner CHECK (created_by = user_id),
    CONSTRAINT fk_feynman_audio_tasks_attempt FOREIGN KEY (attempt_id, user_id)
        REFERENCES feynman_attempts(attempt_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_audio_tasks_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_feynman_audio_tasks_attempt
    ON feynman_audio_tasks (attempt_id, attempt_no DESC);

COMMENT ON TABLE feynman_audio_tasks IS '一次录音上传与 STT 转写结果；写入后不可变（Append-only）';
COMMENT ON COLUMN feynman_audio_tasks.audio_data IS 'v0 音频原始字节直接存 PostgreSQL；录音有大小/时长硬上限，暂不引入对象存储';
COMMENT ON COLUMN feynman_audio_tasks.raw_transcript IS 'STT 原始输出，未经用户确认，不得被下游当作可信文本使用';

CREATE FUNCTION reject_feynman_audio_task_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'feynman_audio_tasks are append-only'
        USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_feynman_audio_tasks_append_only
    BEFORE UPDATE OR DELETE ON feynman_audio_tasks
    FOR EACH ROW EXECUTE FUNCTION reject_feynman_audio_task_mutation();

-- feynman_attempts.active_audio_task_id 必须指向同一 attempt/user 下的音频任务。
ALTER TABLE feynman_attempts
    ADD CONSTRAINT fk_feynman_attempts_active_audio
    FOREIGN KEY (active_audio_task_id, attempt_id, user_id)
    REFERENCES feynman_audio_tasks(audio_task_id, attempt_id, user_id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

-- ---------------------------------------------------------------------------
-- 11.3 转写确认：Append-only，一次录音只能被确认一次。
-- raw_transcript 在这里是确认当时的快照，即使未来音频任务表结构变化也不受影响。
-- ---------------------------------------------------------------------------
CREATE TABLE feynman_transcript_confirmations (
    confirmation_id      UUID         PRIMARY KEY,          -- 后端 UUIDv7
    attempt_id           UUID         NOT NULL,
    audio_task_id        UUID         NOT NULL,
    user_id              VARCHAR(64)  NOT NULL,

    raw_transcript        TEXT         NOT NULL,
    confirmed_transcript   TEXT         NOT NULL,
    edited                 BOOLEAN      NOT NULL,

    confirmed_by          VARCHAR(64)  NOT NULL,
    confirmed_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_feynman_transcript_confirmations_owner
        UNIQUE (confirmation_id, attempt_id, user_id),
    CONSTRAINT uk_feynman_transcript_confirmations_audio UNIQUE (audio_task_id),
    CONSTRAINT ck_feynman_transcript_confirmations_text CHECK (btrim(confirmed_transcript) <> ''),
    CONSTRAINT ck_feynman_transcript_confirmations_confirmed_owner CHECK (confirmed_by = user_id),
    CONSTRAINT fk_feynman_transcript_confirmations_attempt FOREIGN KEY (attempt_id, user_id)
        REFERENCES feynman_attempts(attempt_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_transcript_confirmations_audio FOREIGN KEY (audio_task_id, attempt_id, user_id)
        REFERENCES feynman_audio_tasks(audio_task_id, attempt_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_transcript_confirmations_confirmed_by FOREIGN KEY (confirmed_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

COMMENT ON TABLE feynman_transcript_confirmations IS '用户确认/修正后的转写文本；是下一阶段费曼评估唯一合法输入';
COMMENT ON COLUMN feynman_transcript_confirmations.edited IS '确认文本是否与 STT 原文不同，供统计 STT 准确率与用户修正行为';

CREATE FUNCTION reject_feynman_confirmation_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'feynman_transcript_confirmations are append-only'
        USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_feynman_confirmations_append_only
    BEFORE UPDATE OR DELETE ON feynman_transcript_confirmations
    FOR EACH ROW EXECUTE FUNCTION reject_feynman_confirmation_mutation();

ALTER TABLE feynman_attempts
    ADD CONSTRAINT fk_feynman_attempts_confirmation
    FOREIGN KEY (confirmation_id, attempt_id, user_id)
    REFERENCES feynman_transcript_confirmations(confirmation_id, attempt_id, user_id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

-- ---------------------------------------------------------------------------
-- 11.4 attempts 一旦确认即永久只读；确认前只允许挪动 active_audio_task_id
-- 和写入 confirmation_id（一次性、单向）。归属字段任何时候都不可变。
-- ---------------------------------------------------------------------------
CREATE FUNCTION enforce_feynman_attempt_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.attempt_id IS DISTINCT FROM OLD.attempt_id
        OR NEW.user_id IS DISTINCT FROM OLD.user_id
        OR NEW.knowledge_point_id IS DISTINCT FROM OLD.knowledge_point_id
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.created_by IS DISTINCT FROM OLD.created_by THEN
        RAISE EXCEPTION 'feynman attempt lineage fields are immutable'
            USING ERRCODE = '55000';
    END IF;

    IF OLD.confirmation_id IS NOT NULL THEN
        RAISE EXCEPTION 'feynman attempt % is already confirmed and read-only', OLD.attempt_id
            USING ERRCODE = '55000';
    END IF;

    IF OLD.confirmation_id IS DISTINCT FROM NEW.confirmation_id
        AND NEW.confirmation_id IS NULL THEN
        RAISE EXCEPTION 'feynman attempt confirmation cannot be cleared'
            USING ERRCODE = '55000';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_feynman_attempts_guard
    BEFORE UPDATE ON feynman_attempts
    FOR EACH ROW EXECUTE FUNCTION enforce_feynman_attempt_guard();

-- ---------------------------------------------------------------------------
-- 11.5 知识点版本化 Rubric：Append-only，current_rubric_version_id 指向最新版本。
-- 与 documents / document_versions 的“追加版本 + 延迟外键指针”模式一致。
-- ---------------------------------------------------------------------------
CREATE TABLE knowledge_point_rubrics (
    rubric_id           UUID         PRIMARY KEY,          -- 后端 UUIDv7
    knowledge_point_id  UUID         NOT NULL,
    user_id             VARCHAR(64)  NOT NULL,
    version_no          INTEGER      NOT NULL,
    template_version    VARCHAR(50)  NOT NULL,
    criteria            JSONB        NOT NULL,

    created_by          VARCHAR(64)  NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_knowledge_point_rubrics_owner UNIQUE (rubric_id, knowledge_point_id, user_id),
    CONSTRAINT uk_knowledge_point_rubrics_version UNIQUE (knowledge_point_id, version_no),
    CONSTRAINT ck_knowledge_point_rubrics_version CHECK (version_no > 0),
    CONSTRAINT ck_knowledge_point_rubrics_template CHECK (btrim(template_version) <> ''),
    CONSTRAINT ck_knowledge_point_rubrics_criteria CHECK (
        jsonb_typeof(criteria) = 'array' AND jsonb_array_length(criteria) >= 1
    ),
    CONSTRAINT ck_knowledge_point_rubrics_created_owner CHECK (created_by = user_id),
    CONSTRAINT fk_knowledge_point_rubrics_point FOREIGN KEY (knowledge_point_id, user_id)
        REFERENCES knowledge_points(knowledge_point_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_knowledge_point_rubrics_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_knowledge_point_rubrics_point
    ON knowledge_point_rubrics (knowledge_point_id, version_no DESC);

COMMENT ON TABLE knowledge_point_rubrics IS '知识点的版本化评估 Rubric；只定义评估维度，不产生掌握状态';
COMMENT ON COLUMN knowledge_point_rubrics.criteria IS 'JSON 数组，固定覆盖 5 个维度，结构校验在应用层完成';

CREATE FUNCTION reject_knowledge_point_rubric_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'knowledge_point_rubrics are append-only'
        USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_knowledge_point_rubrics_append_only
    BEFORE UPDATE OR DELETE ON knowledge_point_rubrics
    FOR EACH ROW EXECUTE FUNCTION reject_knowledge_point_rubric_mutation();

ALTER TABLE knowledge_points
    ADD COLUMN current_rubric_version_id UUID;

ALTER TABLE knowledge_points
    ADD CONSTRAINT fk_knowledge_points_current_rubric
    FOREIGN KEY (current_rubric_version_id, knowledge_point_id, user_id)
    REFERENCES knowledge_point_rubrics(rubric_id, knowledge_point_id, user_id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

COMMENT ON COLUMN knowledge_points.current_rubric_version_id IS '当前生效的 Rubric 版本；历史版本永久保留可追溯';
