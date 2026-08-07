-- ============================================================================
-- 000016 · 每日教练数据模型与持久化基础
-- 教练任务是处方当前投影；作答分析和薄弱点快照是追加式事实。
-- 回答正文只保存在 agent_memory_episodic，本迁移只保存 message_id 引用。
-- ============================================================================

-- ---------------------------------------------------------------------------
-- 16.1 每日教练任务：后续计划器写入，本次只建立可领取、可完成的持久化模型。
-- ---------------------------------------------------------------------------
CREATE TABLE coach_daily_tasks (
    coach_task_id       UUID         PRIMARY KEY,
    user_id             VARCHAR(64)  NOT NULL,
    task_date           DATE         NOT NULL,
    task_type           VARCHAR(24)  NOT NULL,
    status              VARCHAR(20)  NOT NULL DEFAULT 'pending',
    source_key          VARCHAR(128) NOT NULL,
    question_text       TEXT         NOT NULL,
    knowledge_point_id  UUID,
    source_gap_id       UUID,
    priority            INTEGER      NOT NULL DEFAULT 100,
    session_id          VARCHAR(64),
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,

    created_by          VARCHAR(64)  NOT NULL,
    updated_by          VARCHAR(64)  NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_coach_daily_tasks_owner UNIQUE (coach_task_id, user_id),
    CONSTRAINT uk_coach_daily_tasks_source UNIQUE (user_id, task_date, source_key),
    CONSTRAINT ck_coach_daily_tasks_type CHECK (task_type IN ('feynman_new', 'feynman_retry')),
    CONSTRAINT ck_coach_daily_tasks_status CHECK (status IN ('pending', 'in_progress', 'completed', 'skipped')),
    CONSTRAINT ck_coach_daily_tasks_source_key CHECK (btrim(source_key) <> ''),
    CONSTRAINT ck_coach_daily_tasks_question CHECK (btrim(question_text) <> ''),
    CONSTRAINT ck_coach_daily_tasks_priority CHECK (priority >= 0),
    CONSTRAINT ck_coach_daily_tasks_times CHECK (
        (status = 'pending' AND session_id IS NULL AND started_at IS NULL AND completed_at IS NULL)
        OR (status = 'in_progress' AND session_id IS NOT NULL AND started_at IS NOT NULL AND completed_at IS NULL)
        OR (status = 'completed' AND session_id IS NOT NULL AND started_at IS NOT NULL AND completed_at IS NOT NULL)
        OR (status = 'skipped' AND completed_at IS NOT NULL)
    ),
    CONSTRAINT ck_coach_daily_tasks_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_coach_daily_tasks_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_coach_daily_tasks_user FOREIGN KEY (user_id)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_daily_tasks_knowledge_point FOREIGN KEY (knowledge_point_id, user_id)
        REFERENCES knowledge_points(knowledge_point_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_daily_tasks_session FOREIGN KEY (session_id, user_id)
        REFERENCES agent_memory_session(session_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_daily_tasks_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_daily_tasks_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_coach_daily_tasks_owner_day
    ON coach_daily_tasks (user_id, task_date DESC, priority, coach_task_id);
CREATE INDEX idx_coach_daily_tasks_pending
    ON coach_daily_tasks (user_id, task_date, priority, coach_task_id)
    WHERE status = 'pending';
CREATE INDEX idx_coach_daily_tasks_session
    ON coach_daily_tasks (session_id, user_id)
    WHERE session_id IS NOT NULL;

CREATE TRIGGER trg_coach_daily_tasks_updated_at
    BEFORE UPDATE ON coach_daily_tasks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 16.2 费曼薄弱点：跨尝试聚合的当前投影；历史证据由 attempt_gaps 保存。
-- ---------------------------------------------------------------------------
CREATE TABLE feynman_gaps (
    gap_id               UUID         PRIMARY KEY,
    user_id               VARCHAR(64)  NOT NULL,
    knowledge_point_id    UUID,
    gap_key               VARCHAR(160) NOT NULL,
    gap_type              VARCHAR(24)  NOT NULL,
    title                 TEXT         NOT NULL,
    description           TEXT         NOT NULL DEFAULT '',
    status                VARCHAR(16)  NOT NULL DEFAULT 'open',
    evidence_count        INTEGER      NOT NULL DEFAULT 1,
    first_seen_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_seen_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    next_review_at        TIMESTAMPTZ,

    created_by            VARCHAR(64)  NOT NULL,
    updated_by            VARCHAR(64)  NOT NULL,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_feynman_gaps_owner UNIQUE (gap_id, user_id),
    CONSTRAINT uk_feynman_gaps_key UNIQUE (user_id, gap_key),
    CONSTRAINT ck_feynman_gaps_key CHECK (btrim(gap_key) <> '' AND gap_key = lower(btrim(gap_key))),
    CONSTRAINT ck_feynman_gaps_type CHECK (gap_type IN (
        'factual_accuracy', 'omission', 'causal_chain', 'project_mapping', 'fact_boundary'
    )),
    CONSTRAINT ck_feynman_gaps_title CHECK (btrim(title) <> ''),
    CONSTRAINT ck_feynman_gaps_status CHECK (status IN ('open', 'resolved')),
    CONSTRAINT ck_feynman_gaps_evidence_count CHECK (evidence_count > 0),
    CONSTRAINT ck_feynman_gaps_seen CHECK (last_seen_at >= first_seen_at),
    CONSTRAINT ck_feynman_gaps_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_feynman_gaps_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_feynman_gaps_user FOREIGN KEY (user_id)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gaps_knowledge_point FOREIGN KEY (knowledge_point_id, user_id)
        REFERENCES knowledge_points(knowledge_point_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gaps_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gaps_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_feynman_gaps_owner_status
    ON feynman_gaps (user_id, status, last_seen_at DESC);
CREATE INDEX idx_feynman_gaps_review_due
    ON feynman_gaps (user_id, next_review_at, gap_id)
    WHERE status = 'open' AND next_review_at IS NOT NULL;
CREATE INDEX idx_feynman_gaps_knowledge_point
    ON feynman_gaps (knowledge_point_id, user_id, last_seen_at DESC)
    WHERE knowledge_point_id IS NOT NULL;

CREATE TRIGGER trg_feynman_gaps_updated_at
    BEFORE UPDATE ON feynman_gaps
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE coach_daily_tasks
    ADD CONSTRAINT fk_coach_daily_tasks_source_gap
    FOREIGN KEY (source_gap_id, user_id)
    REFERENCES feynman_gaps(gap_id, user_id) ON DELETE RESTRICT;

-- ---------------------------------------------------------------------------
-- 16.3 教练作答分析：只允许由 coach_daily_tasks 产生；整行 Append-only。
-- answer_message_id 是答案事实源，original_question_text 和 analysis_payload 是分析时快照。
-- ---------------------------------------------------------------------------
CREATE TABLE coach_attempts (
    coach_attempt_id       UUID         PRIMARY KEY,
    coach_task_id          UUID         NOT NULL,
    user_id                VARCHAR(64)  NOT NULL,
    session_id             VARCHAR(64)  NOT NULL,
    answer_message_id      UUID         NOT NULL,
    original_question_text TEXT         NOT NULL,
    analysis_payload       JSONB        NOT NULL,
    outcome                VARCHAR(20)  NOT NULL,
    prompt_version         VARCHAR(50)  NOT NULL,
    model_name             VARCHAR(100) NOT NULL,
    created_by             VARCHAR(64)  NOT NULL,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_coach_attempts_owner UNIQUE (coach_attempt_id, user_id),
    CONSTRAINT uk_coach_attempts_task_owner UNIQUE (coach_attempt_id, coach_task_id, user_id),
    CONSTRAINT uk_coach_attempts_answer UNIQUE (answer_message_id),
    CONSTRAINT ck_coach_attempts_question CHECK (btrim(original_question_text) <> ''),
    CONSTRAINT ck_coach_attempts_analysis CHECK (jsonb_typeof(analysis_payload) = 'object'),
    CONSTRAINT ck_coach_attempts_outcome CHECK (outcome IN ('passed', 'retry_required')),
    CONSTRAINT ck_coach_attempts_prompt CHECK (btrim(prompt_version) <> ''),
    CONSTRAINT ck_coach_attempts_model CHECK (btrim(model_name) <> ''),
    CONSTRAINT ck_coach_attempts_created_owner CHECK (created_by = user_id),
    CONSTRAINT fk_coach_attempts_task FOREIGN KEY (coach_task_id, user_id)
        REFERENCES coach_daily_tasks(coach_task_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_attempts_session FOREIGN KEY (session_id, user_id)
        REFERENCES agent_memory_session(session_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_attempts_answer FOREIGN KEY (answer_message_id, user_id)
        REFERENCES agent_memory_episodic(message_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_attempts_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_coach_attempts_task_created
    ON coach_attempts (coach_task_id, created_at DESC);
CREATE INDEX idx_coach_attempts_owner_created
    ON coach_attempts (user_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 16.4 尝试-薄弱点快照：保存本次分类输入与判断，整行 Append-only。
-- canonical gap 可继续聚合更新，但历史判断不会随之漂移。
-- ---------------------------------------------------------------------------
CREATE TABLE coach_attempt_gaps (
    attempt_gap_id          UUID         PRIMARY KEY,
    coach_attempt_id        UUID         NOT NULL,
    gap_id                  UUID         NOT NULL,
    user_id                 VARCHAR(64)  NOT NULL,
    gap_key                 VARCHAR(160) NOT NULL,
    gap_type                VARCHAR(24)  NOT NULL,
    classification          VARCHAR(16)  NOT NULL,
    title                   TEXT         NOT NULL,
    description             TEXT         NOT NULL DEFAULT '',
    severity                SMALLINT     NOT NULL,
    is_focus                BOOLEAN      NOT NULL DEFAULT FALSE,
    evidence_payload        JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_coach_attempt_gaps_owner UNIQUE (attempt_gap_id, user_id),
    CONSTRAINT uk_coach_attempt_gaps_gap UNIQUE (coach_attempt_id, gap_id),
    CONSTRAINT uk_coach_attempt_gaps_source UNIQUE (coach_attempt_id, gap_id, user_id),
    CONSTRAINT uk_coach_attempt_gaps_attempt_key UNIQUE (coach_attempt_id, gap_key),
    CONSTRAINT ck_coach_attempt_gaps_key CHECK (btrim(gap_key) <> '' AND gap_key = lower(btrim(gap_key))),
    CONSTRAINT ck_coach_attempt_gaps_type CHECK (gap_type IN (
        'factual_accuracy', 'omission', 'causal_chain', 'project_mapping', 'fact_boundary'
    )),
    CONSTRAINT ck_coach_attempt_gaps_classification CHECK (classification IN ('new', 'recurrent')),
    CONSTRAINT ck_coach_attempt_gaps_title CHECK (btrim(title) <> ''),
    CONSTRAINT ck_coach_attempt_gaps_severity CHECK (severity BETWEEN 1 AND 5),
    CONSTRAINT ck_coach_attempt_gaps_evidence CHECK (jsonb_typeof(evidence_payload) = 'object'),
    CONSTRAINT fk_coach_attempt_gaps_attempt FOREIGN KEY (coach_attempt_id, user_id)
        REFERENCES coach_attempts(coach_attempt_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_attempt_gaps_gap FOREIGN KEY (gap_id, user_id)
        REFERENCES feynman_gaps(gap_id, user_id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX uk_coach_attempt_gaps_focus
    ON coach_attempt_gaps (coach_attempt_id)
    WHERE is_focus;
CREATE INDEX idx_coach_attempt_gaps_gap_created
    ON coach_attempt_gaps (gap_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 16.5 薄弱点复习排程：可由后续调度器领取并完成；来源尝试不可变。
-- ---------------------------------------------------------------------------
CREATE TABLE feynman_gap_reviews (
    gap_review_id          UUID         PRIMARY KEY,
    gap_id                 UUID         NOT NULL,
    source_attempt_id      UUID         NOT NULL,
    user_id                VARCHAR(64)  NOT NULL,
    scheduled_for          TIMESTAMPTZ  NOT NULL,
    status                 VARCHAR(16)  NOT NULL DEFAULT 'scheduled',
    completed_attempt_id   UUID,
    completed_at           TIMESTAMPTZ,
    created_by             VARCHAR(64)  NOT NULL,
    updated_by             VARCHAR(64)  NOT NULL,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_feynman_gap_reviews_owner UNIQUE (gap_review_id, user_id),
    CONSTRAINT uk_feynman_gap_reviews_schedule UNIQUE (gap_id, source_attempt_id, scheduled_for),
    CONSTRAINT uk_feynman_gap_reviews_completed UNIQUE (completed_attempt_id),
    CONSTRAINT ck_feynman_gap_reviews_status CHECK (status IN ('scheduled', 'completed', 'cancelled')),
    CONSTRAINT ck_feynman_gap_reviews_completion CHECK (
        (status = 'scheduled' AND completed_attempt_id IS NULL AND completed_at IS NULL)
        OR (status = 'completed' AND completed_attempt_id IS NOT NULL AND completed_at IS NOT NULL)
        OR (status = 'cancelled' AND completed_attempt_id IS NULL AND completed_at IS NOT NULL)
    ),
    CONSTRAINT ck_feynman_gap_reviews_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_feynman_gap_reviews_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_feynman_gap_reviews_gap FOREIGN KEY (gap_id, user_id)
        REFERENCES feynman_gaps(gap_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_reviews_source_attempt FOREIGN KEY (source_attempt_id, user_id)
        REFERENCES coach_attempts(coach_attempt_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_reviews_source_gap FOREIGN KEY (source_attempt_id, gap_id, user_id)
        REFERENCES coach_attempt_gaps(coach_attempt_id, gap_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_reviews_completed_attempt FOREIGN KEY (completed_attempt_id, user_id)
        REFERENCES coach_attempts(coach_attempt_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_reviews_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_reviews_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_feynman_gap_reviews_due
    ON feynman_gap_reviews (user_id, scheduled_for, gap_review_id)
    WHERE status = 'scheduled';
CREATE INDEX idx_feynman_gap_reviews_gap
    ON feynman_gap_reviews (gap_id, scheduled_for DESC);

CREATE TRIGGER trg_feynman_gap_reviews_updated_at
    BEFORE UPDATE ON feynman_gap_reviews
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- attempts 与 attempt_gaps 是分析事实快照，数据库层拒绝 UPDATE/DELETE。
CREATE FUNCTION reject_coach_snapshot_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_coach_attempts_append_only
    BEFORE UPDATE OR DELETE ON coach_attempts
    FOR EACH ROW EXECUTE FUNCTION reject_coach_snapshot_mutation();
CREATE TRIGGER trg_coach_attempt_gaps_append_only
    BEFORE UPDATE OR DELETE ON coach_attempt_gaps
    FOR EACH ROW EXECUTE FUNCTION reject_coach_snapshot_mutation();

-- ---------------------------------------------------------------------------
-- 16.6 扩展会话级费曼投影；默认值保持既有自由费曼行为不变。
-- ---------------------------------------------------------------------------
ALTER TABLE feynman_practice_states
    DROP CONSTRAINT ck_feynman_practice_states_state,
    DROP CONSTRAINT ck_feynman_practice_states_origin,
    DROP CONSTRAINT ck_feynman_practice_states_question;

ALTER TABLE feynman_practice_states
    ADD COLUMN coach_task_id UUID,
    ADD COLUMN original_question_text TEXT NOT NULL DEFAULT '',
    ADD COLUMN retry_required BOOLEAN NOT NULL DEFAULT FALSE,
    ADD CONSTRAINT ck_feynman_practice_states_state CHECK (state IN (
        'idle', 'awaiting_topic', 'awaiting_answer', 'analyzing_answer',
        'awaiting_follow_up', 'awaiting_retry', 'queue_paused'
    )),
    ADD CONSTRAINT ck_feynman_practice_states_origin CHECK (
        question_origin IN ('', 'user_topic', 'ai_follow_up', 'coach_task')
    ),
    ADD CONSTRAINT ck_feynman_practice_states_question CHECK (
        state NOT IN ('awaiting_answer', 'awaiting_retry') OR btrim(active_question_text) <> ''
    ),
    ADD CONSTRAINT ck_feynman_practice_states_coach CHECK (
        (question_origin = 'coach_task' AND coach_task_id IS NOT NULL AND btrim(original_question_text) <> '')
        OR (question_origin <> 'coach_task' AND coach_task_id IS NULL AND original_question_text = '' AND NOT retry_required)
    ),
    ADD CONSTRAINT ck_feynman_practice_states_retry CHECK (
        (state = 'awaiting_retry') = retry_required
    ),
    ADD CONSTRAINT fk_feynman_practice_states_coach_task FOREIGN KEY (coach_task_id, user_id)
        REFERENCES coach_daily_tasks(coach_task_id, user_id) ON DELETE RESTRICT;

CREATE INDEX idx_feynman_practice_states_coach_task
    ON feynman_practice_states (coach_task_id, user_id)
    WHERE coach_task_id IS NOT NULL;

COMMENT ON TABLE coach_daily_tasks IS '每日教练处方任务当前投影；计划生成不在本迁移范围';
COMMENT ON TABLE coach_attempts IS '仅教练处方任务的作答分析快照；答案正文由 answer_message_id 指向 agent_memory_episodic';
COMMENT ON COLUMN coach_attempts.analysis_payload IS '结构化、已清除回答原文等敏感冗余字段的分析 JSON 快照';
COMMENT ON TABLE feynman_gaps IS '跨教练尝试聚合的费曼薄弱点当前投影';
COMMENT ON TABLE coach_attempt_gaps IS '某次教练分析识别出的全部薄弱点及唯一 focus 快照；Append-only';
COMMENT ON TABLE feynman_gap_reviews IS '薄弱点复习排程钩子；由后续调度切片领取和完成';
COMMENT ON COLUMN feynman_practice_states.coach_task_id IS '非空表示当前练习由每日教练任务启动；自由费曼保持 NULL';
COMMENT ON COLUMN feynman_practice_states.original_question_text IS '教练任务开始时的原题快照，重试时保持不变';
COMMENT ON COLUMN feynman_practice_states.retry_required IS '分析后是否必须重答；为 true 时 state 必须为 awaiting_retry';
