-- ============================================================================
-- 000013 · 费曼评估与证据候选
-- AI 只生成 proposed 评估；用户决策单独 Append-only 保存，不改掌握状态。
-- ============================================================================

CREATE TABLE feynman_evaluations (
    evaluation_id       UUID         PRIMARY KEY,
    attempt_id          UUID         NOT NULL,
    confirmation_id     UUID         NOT NULL,
    rubric_id           UUID         NOT NULL,
    knowledge_point_id  UUID         NOT NULL,
    user_id             VARCHAR(64)  NOT NULL,
    status              VARCHAR(20)  NOT NULL,
    prompt_version      VARCHAR(50)  NOT NULL,
    model_name          VARCHAR(100) NOT NULL,
    retrieval_request_id UUID,
    confirmed_transcript_hash BYTEA  NOT NULL,
    result_payload      JSONB,
    source_snapshots    JSONB        NOT NULL DEFAULT '[]'::jsonb,
    error_message       TEXT,
    created_by          VARCHAR(64)  NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_feynman_evaluations_attempt UNIQUE (attempt_id),
    CONSTRAINT uk_feynman_evaluations_owner UNIQUE (evaluation_id, user_id),
    CONSTRAINT ck_feynman_evaluations_status CHECK (status IN ('evaluating', 'proposed', 'failed')),
    CONSTRAINT ck_feynman_evaluations_hash CHECK (octet_length(confirmed_transcript_hash) = 32),
    CONSTRAINT ck_feynman_evaluations_terminal CHECK (
        (status = 'evaluating' AND result_payload IS NULL AND error_message IS NULL)
        OR (status = 'proposed' AND result_payload IS NOT NULL AND error_message IS NULL)
        OR (status = 'failed' AND result_payload IS NULL AND error_message IS NOT NULL)
    ),
    CONSTRAINT ck_feynman_evaluations_created_owner CHECK (created_by = user_id),
    CONSTRAINT fk_feynman_evaluations_attempt FOREIGN KEY (attempt_id, user_id)
        REFERENCES feynman_attempts(attempt_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_evaluations_confirmation FOREIGN KEY (confirmation_id, attempt_id, user_id)
        REFERENCES feynman_transcript_confirmations(confirmation_id, attempt_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_evaluations_rubric FOREIGN KEY (rubric_id, knowledge_point_id, user_id)
        REFERENCES knowledge_point_rubrics(rubric_id, knowledge_point_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_evaluations_retrieval FOREIGN KEY (retrieval_request_id)
        REFERENCES retrieval_requests(retrieval_request_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_evaluations_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_feynman_evaluations_user_created
    ON feynman_evaluations (user_id, created_at DESC);

CREATE TABLE feynman_evaluation_decisions (
    decision_id       UUID         PRIMARY KEY,
    evaluation_id     UUID         NOT NULL,
    user_id           VARCHAR(64)  NOT NULL,
    decision          VARCHAR(20)  NOT NULL,
    final_payload     JSONB,
    decision_note     TEXT,
    decided_by        VARCHAR(64)  NOT NULL,
    decided_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_feynman_evaluation_decisions_evaluation UNIQUE (evaluation_id),
    CONSTRAINT ck_feynman_evaluation_decisions_decision CHECK (decision IN ('confirmed', 'rejected')),
    CONSTRAINT ck_feynman_evaluation_decisions_payload CHECK (
        (decision = 'confirmed' AND final_payload IS NOT NULL)
        OR (decision = 'rejected' AND final_payload IS NULL)
    ),
    CONSTRAINT ck_feynman_evaluation_decisions_owner CHECK (decided_by = user_id),
    CONSTRAINT fk_feynman_evaluation_decisions_evaluation FOREIGN KEY (evaluation_id, user_id)
        REFERENCES feynman_evaluations(evaluation_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_evaluation_decisions_user FOREIGN KEY (decided_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE FUNCTION guard_feynman_evaluation_transition() RETURNS trigger AS $$
BEGIN
    IF NEW.evaluation_id IS DISTINCT FROM OLD.evaluation_id
        OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id
        OR NEW.confirmation_id IS DISTINCT FROM OLD.confirmation_id
        OR NEW.rubric_id IS DISTINCT FROM OLD.rubric_id
        OR NEW.knowledge_point_id IS DISTINCT FROM OLD.knowledge_point_id
        OR NEW.user_id IS DISTINCT FROM OLD.user_id
        OR NEW.prompt_version IS DISTINCT FROM OLD.prompt_version
        OR NEW.model_name IS DISTINCT FROM OLD.model_name
        OR NEW.confirmed_transcript_hash IS DISTINCT FROM OLD.confirmed_transcript_hash
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'feynman evaluation lineage is immutable' USING ERRCODE = '55000';
    END IF;
    IF NOT (
        (OLD.status = 'evaluating' AND NEW.status IN ('proposed', 'failed'))
        OR (OLD.status = 'failed' AND NEW.status = 'evaluating')
    ) THEN
        RAISE EXCEPTION 'invalid feynman evaluation transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = '55000';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_feynman_evaluations_guard
    BEFORE UPDATE ON feynman_evaluations
    FOR EACH ROW EXECUTE FUNCTION guard_feynman_evaluation_transition();

CREATE FUNCTION reject_feynman_evaluation_decision_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'feynman evaluation decisions are append-only' USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_feynman_evaluation_decisions_append_only
    BEFORE UPDATE OR DELETE ON feynman_evaluation_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_feynman_evaluation_decision_mutation();

COMMENT ON TABLE feynman_evaluations IS '版本固定、引用可复现的费曼评估提案；不直接改变掌握状态';
COMMENT ON TABLE feynman_evaluation_decisions IS '用户对评估与证据候选的确认、修改后确认或拒绝';