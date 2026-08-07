DROP TRIGGER IF EXISTS trg_coach_task_pending_gaps_updated_at ON coach_task_pending_gaps;
DROP TABLE coach_task_pending_gaps;

DROP INDEX idx_feynman_gap_reviews_cycle;
DROP INDEX idx_feynman_gap_reviews_due_date;

ALTER TABLE feynman_gap_reviews
    DROP CONSTRAINT uk_feynman_gap_reviews_cycle_stage,
    DROP CONSTRAINT fk_feynman_gap_reviews_cycle_gap,
    DROP CONSTRAINT fk_feynman_gap_reviews_cycle_owner,
    DROP CONSTRAINT ck_feynman_gap_reviews_completion,
    DROP CONSTRAINT ck_feynman_gap_reviews_stage,
    DROP CONSTRAINT ck_feynman_gap_reviews_status;

-- 先清理将被强制 skipped 的活动任务对应 practice；否则 task/practice 会产生相互矛盾的投影。
UPDATE feynman_practice_states AS practice
SET state = 'idle',
    active_question_text = '',
    question_origin = '',
    coach_task_id = NULL,
    original_question_text = '',
    retry_required = FALSE,
    last_answered_message_id = NULL,
    last_feedback = '',
    round_no = 0
FROM coach_daily_tasks AS task
JOIN feynman_gap_reviews AS review
  ON review.gap_review_id = task.source_review_id
WHERE practice.coach_task_id = task.coach_task_id
  AND practice.user_id = task.user_id
  AND review.status IN ('failed', 'missed')
  AND task.status <> 'skipped';

-- 000017 的 source_review 唯一条件只排除 skipped。failed/missed 降回 scheduled 前，
-- 把其非 skipped 任务转为 skipped，避免旧 planner 永久屏蔽已恢复的 scheduled review。
UPDATE coach_daily_tasks AS task
SET status = 'skipped',
    completed_at = COALESCE(task.completed_at, now()),
    updated_by = task.user_id
FROM feynman_gap_reviews AS review
WHERE task.source_review_id = review.gap_review_id
  AND review.status IN ('failed', 'missed')
  AND task.status <> 'skipped';

-- 旧模型没有 passed/failed/missed。passed 保留为 completed；failed/missed 恢复为
-- scheduled 并清除完成字段，cancelled 保留。这样每行都满足旧 completion CHECK。
UPDATE feynman_gap_reviews
SET status = CASE
        WHEN status = 'passed' AND completed_attempt_id IS NOT NULL THEN 'completed'
        WHEN status = 'cancelled' THEN 'cancelled'
        ELSE 'scheduled'
    END,
    completed_attempt_id = CASE
        WHEN status = 'passed' AND completed_attempt_id IS NOT NULL THEN completed_attempt_id
        ELSE NULL
    END,
    completed_at = CASE
        WHEN status = 'passed' AND completed_attempt_id IS NOT NULL THEN COALESCE(completed_at, now())
        WHEN status = 'cancelled' THEN COALESCE(completed_at, now())
        ELSE NULL
    END;

ALTER TABLE feynman_gap_reviews
    DROP COLUMN scheduled_date,
    DROP COLUMN stage,
    DROP COLUMN review_cycle_id,
    ADD CONSTRAINT ck_feynman_gap_reviews_status CHECK (status IN ('scheduled', 'completed', 'cancelled')),
    ADD CONSTRAINT ck_feynman_gap_reviews_completion CHECK (
        (status = 'scheduled' AND completed_attempt_id IS NULL AND completed_at IS NULL)
        OR (status = 'completed' AND completed_attempt_id IS NOT NULL AND completed_at IS NOT NULL)
        OR (status = 'cancelled' AND completed_attempt_id IS NULL AND completed_at IS NOT NULL)
    );

CREATE INDEX idx_feynman_gap_reviews_due
    ON feynman_gap_reviews (user_id, scheduled_for, gap_review_id)
    WHERE status = 'scheduled';
CREATE INDEX idx_feynman_gap_reviews_plan_due
    ON feynman_gap_reviews (user_id, scheduled_for, gap_review_id)
    WHERE status = 'scheduled';

DROP TRIGGER IF EXISTS trg_feynman_gap_review_cycles_updated_at ON feynman_gap_review_cycles;
DROP TABLE feynman_gap_review_cycles;
