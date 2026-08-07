-- ============================================================================
-- 000017 · 每日教练计划角色与复习来源
-- 在 000016 基础上补齐 required/optional 显式语义和 review 级来源身份。
-- ============================================================================

ALTER TABLE coach_daily_tasks
    ADD COLUMN plan_role VARCHAR(16) NOT NULL DEFAULT 'optional',
    ADD COLUMN source_review_id UUID;

-- 000016 允许仅带 source_gap_id 的 legacy feynman_retry。000017 引入 review 级来源后，
-- 这些行无法可靠反推出具体 review；保留历史任务身份并只对新式完整来源施加强约束。
ALTER TABLE coach_daily_tasks
    ADD CONSTRAINT ck_coach_daily_tasks_plan_role CHECK (plan_role IN ('required', 'optional')),
    ADD CONSTRAINT fk_coach_daily_tasks_source_review FOREIGN KEY (source_review_id, user_id)
        REFERENCES feynman_gap_reviews(gap_review_id, user_id) ON DELETE RESTRICT,
    ADD CONSTRAINT ck_coach_daily_tasks_review_source CHECK (
        (task_type = 'feynman_retry' AND source_gap_id IS NOT NULL)
        OR (task_type = 'feynman_new' AND source_gap_id IS NULL AND source_review_id IS NULL)
    );

-- 同一用户不能同时进行两条 Coach 任务，避免 GET 生成竞争任务。
CREATE UNIQUE INDEX uk_coach_daily_tasks_one_in_progress
    ON coach_daily_tasks (user_id)
    WHERE status = 'in_progress';

-- 一个 review 只能落成一条 Coach 任务；重放或跨日期读取都不会复制任务。
CREATE UNIQUE INDEX uk_coach_daily_tasks_source_review
    ON coach_daily_tasks (source_review_id)
    WHERE source_review_id IS NOT NULL;

-- 每日计划最多一个 required；其余最多两条由仓储在 advisory lock 内控制。
CREATE UNIQUE INDEX uk_coach_daily_tasks_required_per_day
    ON coach_daily_tasks (user_id, task_date)
    WHERE plan_role = 'required' AND status IN ('pending', 'in_progress');

CREATE INDEX idx_coach_daily_tasks_progress
    ON coach_daily_tasks (user_id, task_date, status, plan_role);

CREATE INDEX idx_feynman_gap_reviews_plan_due
    ON feynman_gap_reviews (user_id, scheduled_for, gap_review_id)
    WHERE status = 'scheduled';

COMMENT ON COLUMN coach_daily_tasks.plan_role IS '每日计划角色：恰好第一条为 required，其余最多两条为 optional';
COMMENT ON COLUMN coach_daily_tasks.source_review_id IS '重试任务对应的具体复习排程；保证同一 review 不会重复生成任务';
