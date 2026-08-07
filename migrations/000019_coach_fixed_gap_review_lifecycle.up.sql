-- ============================================================================
-- 000019 · Coach 固定 +1/+3/+7 薄弱点复测生命周期
-- 增加显式周期、阶段、本地日期、结果状态，以及任务内待纠正薄弱点的持久化关联。
-- ============================================================================

CREATE TABLE feynman_gap_review_cycles (
    review_cycle_id       UUID         PRIMARY KEY,
    gap_id                UUID         NOT NULL,
    source_attempt_id     UUID         NOT NULL,
    correction_attempt_id UUID,
    coach_task_id         UUID         NOT NULL,
    user_id               VARCHAR(64)  NOT NULL,
    anchor_date           DATE         NOT NULL,

    created_by            VARCHAR(64)  NOT NULL,
    updated_by            VARCHAR(64)  NOT NULL,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_feynman_gap_review_cycles_owner UNIQUE (review_cycle_id, user_id),
    CONSTRAINT uk_feynman_gap_review_cycles_gap_owner UNIQUE (review_cycle_id, gap_id, user_id),
    CONSTRAINT uk_feynman_gap_review_cycles_correction UNIQUE (gap_id, correction_attempt_id),
    CONSTRAINT ck_feynman_gap_review_cycles_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_feynman_gap_review_cycles_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_feynman_gap_review_cycles_gap FOREIGN KEY (gap_id, user_id)
        REFERENCES feynman_gaps(gap_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_review_cycles_source_gap FOREIGN KEY (source_attempt_id, gap_id, user_id)
        REFERENCES coach_attempt_gaps(coach_attempt_id, gap_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_review_cycles_task FOREIGN KEY (coach_task_id, user_id)
        REFERENCES coach_daily_tasks(coach_task_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_review_cycles_source_task FOREIGN KEY (source_attempt_id, coach_task_id, user_id)
        REFERENCES coach_attempts(coach_attempt_id, coach_task_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_review_cycles_correction_task FOREIGN KEY (correction_attempt_id, coach_task_id, user_id)
        REFERENCES coach_attempts(coach_attempt_id, coach_task_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_review_cycles_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_gap_review_cycles_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE TRIGGER trg_feynman_gap_review_cycles_updated_at
    BEFORE UPDATE ON feynman_gap_review_cycles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 历史 review 没有周期身份。每条历史 review 自成一个 stage=1 周期，使用自身 UUID
-- 作为 cycle UUID；这样不改动任何既有 owner/source lineage，也不会猜测历史分组。
INSERT INTO feynman_gap_review_cycles (
    review_cycle_id, gap_id, source_attempt_id, correction_attempt_id, coach_task_id, user_id,
    anchor_date, created_by, updated_by, created_at, updated_at
)
SELECT review.gap_review_id, review.gap_id, review.source_attempt_id, NULL,
       attempt.coach_task_id, review.user_id,
       task.task_date, review.created_by, review.updated_by,
       review.created_at, review.updated_at
FROM feynman_gap_reviews AS review
JOIN coach_attempts AS attempt
  ON attempt.coach_attempt_id = review.source_attempt_id
 AND attempt.user_id = review.user_id
JOIN coach_daily_tasks AS task
  ON task.coach_task_id = attempt.coach_task_id
 AND task.user_id = attempt.user_id;

ALTER TABLE feynman_gap_reviews
    ADD COLUMN review_cycle_id UUID,
    ADD COLUMN stage SMALLINT,
    ADD COLUMN scheduled_date DATE,
    DROP CONSTRAINT ck_feynman_gap_reviews_status,
    DROP CONSTRAINT ck_feynman_gap_reviews_completion;

UPDATE feynman_gap_reviews AS review
SET review_cycle_id = review.gap_review_id,
    stage = 1,
    scheduled_date = task.task_date + 1,
    status = CASE WHEN review.status = 'completed' THEN 'passed' ELSE review.status END
FROM coach_attempts AS attempt
JOIN coach_daily_tasks AS task
  ON task.coach_task_id = attempt.coach_task_id
 AND task.user_id = attempt.user_id
WHERE attempt.coach_attempt_id = review.source_attempt_id
  AND attempt.user_id = review.user_id;

ALTER TABLE feynman_gap_reviews
    ALTER COLUMN review_cycle_id SET NOT NULL,
    ALTER COLUMN stage SET NOT NULL,
    ALTER COLUMN scheduled_date SET NOT NULL,
    ADD CONSTRAINT ck_feynman_gap_reviews_status CHECK (
        status IN ('scheduled', 'passed', 'failed', 'missed', 'cancelled')
    ),
    ADD CONSTRAINT ck_feynman_gap_reviews_stage CHECK (stage IN (1, 2, 3)),
    ADD CONSTRAINT ck_feynman_gap_reviews_completion CHECK (
        (status = 'scheduled' AND completed_attempt_id IS NULL AND completed_at IS NULL)
        OR (status IN ('passed', 'failed') AND completed_attempt_id IS NOT NULL AND completed_at IS NOT NULL)
        OR (status IN ('missed', 'cancelled') AND completed_attempt_id IS NULL AND completed_at IS NOT NULL)
    ),
    ADD CONSTRAINT fk_feynman_gap_reviews_cycle_owner FOREIGN KEY (review_cycle_id, user_id)
        REFERENCES feynman_gap_review_cycles(review_cycle_id, user_id) ON DELETE RESTRICT,
    ADD CONSTRAINT fk_feynman_gap_reviews_cycle_gap FOREIGN KEY (review_cycle_id, gap_id, user_id)
        REFERENCES feynman_gap_review_cycles(review_cycle_id, gap_id, user_id) ON DELETE RESTRICT,
    ADD CONSTRAINT uk_feynman_gap_reviews_cycle_stage UNIQUE (review_cycle_id, stage);

DROP INDEX idx_feynman_gap_reviews_due;
DROP INDEX idx_feynman_gap_reviews_plan_due;
CREATE INDEX idx_feynman_gap_reviews_due_date
    ON feynman_gap_reviews (user_id, scheduled_date, gap_review_id)
    WHERE status IN ('scheduled', 'missed');
CREATE INDEX idx_feynman_gap_reviews_cycle
    ON feynman_gap_reviews (review_cycle_id, stage);

-- 任务内的待纠正集合是“失败分析 -> 后续通过重答 -> 建周期”的可靠持久化桥梁。
-- 它避免通过当前回答文本或当前 gap 列表反推此前需要纠正的薄弱点。
CREATE TABLE coach_task_pending_gaps (
    coach_task_id          UUID         NOT NULL,
    gap_id                 UUID         NOT NULL,
    user_id                VARCHAR(64)  NOT NULL,
    detected_attempt_id    UUID         NOT NULL,
    corrected_attempt_id   UUID,
    status                 VARCHAR(16)  NOT NULL DEFAULT 'pending',

    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT pk_coach_task_pending_gaps PRIMARY KEY (coach_task_id, gap_id),
    CONSTRAINT ck_coach_task_pending_gaps_status CHECK (status IN ('pending', 'corrected')),
    CONSTRAINT ck_coach_task_pending_gaps_correction CHECK (
        (status = 'pending' AND corrected_attempt_id IS NULL)
        OR (status = 'corrected' AND corrected_attempt_id IS NOT NULL)
    ),
    CONSTRAINT fk_coach_task_pending_gaps_task FOREIGN KEY (coach_task_id, user_id)
        REFERENCES coach_daily_tasks(coach_task_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_task_pending_gaps_gap FOREIGN KEY (gap_id, user_id)
        REFERENCES feynman_gaps(gap_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_task_pending_gaps_detected_attempt FOREIGN KEY (detected_attempt_id, coach_task_id, user_id)
        REFERENCES coach_attempts(coach_attempt_id, coach_task_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_coach_task_pending_gaps_corrected_attempt FOREIGN KEY (corrected_attempt_id, coach_task_id, user_id)
        REFERENCES coach_attempts(coach_attempt_id, coach_task_id, user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_coach_task_pending_gaps_pending
    ON coach_task_pending_gaps (coach_task_id, user_id, gap_id)
    WHERE status = 'pending';
CREATE TRIGGER trg_coach_task_pending_gaps_updated_at
    BEFORE UPDATE ON coach_task_pending_gaps
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE feynman_gap_review_cycles IS '薄弱点固定 +1/+3/+7 复测周期；anchor_date 使用实际纠正通过本地日期，历史行回退来源任务日期';
COMMENT ON COLUMN feynman_gap_review_cycles.correction_attempt_id IS '创建周期所依据的通过重答；历史迁移周期可为空';
COMMENT ON COLUMN feynman_gap_review_cycles.coach_task_id IS '发现与纠正 attempt 所属 Coach 任务；复合外键防止跨任务 lineage';
COMMENT ON COLUMN feynman_gap_reviews.stage IS '固定周期阶段：1/2/3 分别对应 anchor_date +1/+3/+7';
COMMENT ON COLUMN feynman_gap_reviews.scheduled_date IS '用户本地 DATE 到期日；每日计划只按此列选择';
COMMENT ON TABLE coach_task_pending_gaps IS '任务内等待通过原题重答后建立复测周期的 canonical gap 集合';
