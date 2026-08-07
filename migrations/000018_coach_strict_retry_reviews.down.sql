DROP INDEX uk_coach_daily_tasks_source_review_active;

-- 000018 允许跳过同一 review 后重新生成任务，因此回滚时可能已有多个相同 source_review_id。
-- 旧索引重新要求全局唯一：优先保留未跳过的投影；若全部已跳过，则保留最新的一条。
-- 其余历史任务不能直接删除（可能已被 attempt 引用），故转成旧模型允许的独立新题并清除 review/gap 来源。
WITH ranked_review_tasks AS (
    SELECT coach_task_id,
           row_number() OVER (
               PARTITION BY source_review_id
               ORDER BY (status <> 'skipped') DESC, created_at DESC, coach_task_id DESC
           ) AS source_rank
    FROM coach_daily_tasks
    WHERE source_review_id IS NOT NULL
)
UPDATE coach_daily_tasks AS task
SET task_type = 'feynman_new',
    source_key = 'rollback-detached-review:' || task.coach_task_id::text,
    source_gap_id = NULL,
    source_review_id = NULL,
    updated_by = task.user_id
FROM ranked_review_tasks AS ranked
WHERE task.coach_task_id = ranked.coach_task_id
  AND ranked.source_rank > 1;

CREATE UNIQUE INDEX uk_coach_daily_tasks_source_review
    ON coach_daily_tasks (source_review_id)
    WHERE source_review_id IS NOT NULL;

ALTER TABLE feynman_practice_states DROP CONSTRAINT ck_feynman_practice_states_retry;
-- 03A 允许“暂停中的强制重答”保留 retry_required=true；旧 CHECK 不允许该组合。
UPDATE feynman_practice_states
SET retry_required = FALSE
WHERE state = 'queue_paused' AND retry_required;
ALTER TABLE feynman_practice_states
    ADD CONSTRAINT ck_feynman_practice_states_retry CHECK ((state = 'awaiting_retry') = retry_required);

DROP INDEX uk_coach_daily_tasks_one_active;
DROP INDEX uk_coach_daily_tasks_required_per_day;
CREATE UNIQUE INDEX uk_coach_daily_tasks_required_per_day
    ON coach_daily_tasks (user_id, task_date)
    WHERE plan_role = 'required' AND status IN ('pending', 'in_progress');
UPDATE coach_daily_tasks
SET status = 'in_progress'
WHERE status = 'awaiting_retry';
ALTER TABLE coach_daily_tasks
    DROP CONSTRAINT ck_coach_daily_tasks_times,
    DROP CONSTRAINT ck_coach_daily_tasks_status,
    ADD CONSTRAINT ck_coach_daily_tasks_status CHECK (status IN ('pending', 'in_progress', 'completed', 'skipped')),
    ADD CONSTRAINT ck_coach_daily_tasks_times CHECK (
        (status = 'pending' AND session_id IS NULL AND started_at IS NULL AND completed_at IS NULL)
        OR (status = 'in_progress' AND session_id IS NOT NULL AND started_at IS NOT NULL AND completed_at IS NULL)
        OR (status = 'completed' AND session_id IS NOT NULL AND started_at IS NOT NULL AND completed_at IS NOT NULL)
        OR (status = 'skipped' AND completed_at IS NOT NULL)
    );
CREATE UNIQUE INDEX uk_coach_daily_tasks_one_in_progress
    ON coach_daily_tasks (user_id)
    WHERE status = 'in_progress';

ALTER TABLE coach_attempt_gaps
    DROP CONSTRAINT ck_coach_attempt_gaps_dimension,
    DROP CONSTRAINT ck_coach_attempt_gaps_type;
ALTER TABLE feynman_gaps
    DROP CONSTRAINT ck_feynman_gaps_dimension,
    DROP CONSTRAINT ck_feynman_gaps_type;

-- 新诊断维度必须映射回 000016 约束允许的旧 gap_type；expression 在旧模型中归入 omission。
DROP TRIGGER trg_coach_attempt_gaps_append_only ON coach_attempt_gaps;
UPDATE coach_attempt_gaps
SET gap_type = CASE diagnostic_dimension
        WHEN 'expression' THEN 'omission'
        WHEN 'key_points' THEN 'omission'
        WHEN 'causal_chain' THEN 'causal_chain'
        WHEN 'project_mapping' THEN 'project_mapping'
        WHEN 'fact_boundary' THEN 'fact_boundary'
    END;
CREATE TRIGGER trg_coach_attempt_gaps_append_only
    BEFORE UPDATE OR DELETE ON coach_attempt_gaps
    FOR EACH ROW EXECUTE FUNCTION reject_coach_snapshot_mutation();
UPDATE feynman_gaps
SET gap_type = CASE diagnostic_dimension
        WHEN 'expression' THEN 'omission'
        WHEN 'key_points' THEN 'omission'
        WHEN 'causal_chain' THEN 'causal_chain'
        WHEN 'project_mapping' THEN 'project_mapping'
        WHEN 'fact_boundary' THEN 'fact_boundary'
    END;

ALTER TABLE coach_attempt_gaps
    DROP COLUMN diagnostic_dimension,
    ADD CONSTRAINT ck_coach_attempt_gaps_type CHECK (gap_type IN (
        'factual_accuracy', 'omission', 'causal_chain', 'project_mapping', 'fact_boundary'
    ));
ALTER TABLE feynman_gaps
    DROP COLUMN diagnostic_dimension,
    ADD CONSTRAINT ck_feynman_gaps_type CHECK (gap_type IN (
        'factual_accuracy', 'omission', 'causal_chain', 'project_mapping', 'fact_boundary'
    ));
