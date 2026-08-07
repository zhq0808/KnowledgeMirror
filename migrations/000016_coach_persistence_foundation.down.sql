ALTER TABLE feynman_practice_states
    DROP CONSTRAINT IF EXISTS fk_feynman_practice_states_coach_task,
    DROP CONSTRAINT IF EXISTS ck_feynman_practice_states_retry,
    DROP CONSTRAINT IF EXISTS ck_feynman_practice_states_coach,
    DROP CONSTRAINT IF EXISTS ck_feynman_practice_states_question,
    DROP CONSTRAINT IF EXISTS ck_feynman_practice_states_origin,
    DROP CONSTRAINT IF EXISTS ck_feynman_practice_states_state;

DROP INDEX IF EXISTS idx_feynman_practice_states_coach_task;

-- 回滚前先把新状态归一到旧状态机可接受的 idle；否则 awaiting_retry/coach_task
-- 会在恢复旧 CHECK 时阻断整个 down migration。
UPDATE feynman_practice_states
SET state = 'idle',
    active_question_text = '',
    question_origin = '',
    last_answered_message_id = NULL,
    last_feedback = '',
    coach_task_id = NULL,
    original_question_text = '',
    retry_required = FALSE
WHERE coach_task_id IS NOT NULL
   OR question_origin = 'coach_task'
   OR state = 'awaiting_retry'
   OR retry_required;

ALTER TABLE feynman_practice_states
    DROP COLUMN IF EXISTS retry_required,
    DROP COLUMN IF EXISTS original_question_text,
    DROP COLUMN IF EXISTS coach_task_id,
    ADD CONSTRAINT ck_feynman_practice_states_state CHECK (state IN (
        'idle', 'awaiting_topic', 'awaiting_answer', 'analyzing_answer',
        'awaiting_follow_up', 'queue_paused'
    )),
    ADD CONSTRAINT ck_feynman_practice_states_origin CHECK (
        question_origin IN ('', 'user_topic', 'ai_follow_up')
    ),
    ADD CONSTRAINT ck_feynman_practice_states_question CHECK (
        state <> 'awaiting_answer' OR btrim(active_question_text) <> ''
    );

DROP TRIGGER IF EXISTS trg_feynman_gap_reviews_updated_at ON feynman_gap_reviews;
DROP TABLE IF EXISTS feynman_gap_reviews;

DROP TRIGGER IF EXISTS trg_coach_attempt_gaps_append_only ON coach_attempt_gaps;
DROP TABLE IF EXISTS coach_attempt_gaps;

DROP TRIGGER IF EXISTS trg_coach_attempts_append_only ON coach_attempts;
DROP TABLE IF EXISTS coach_attempts;

DROP FUNCTION IF EXISTS reject_coach_snapshot_mutation();

ALTER TABLE coach_daily_tasks DROP CONSTRAINT IF EXISTS fk_coach_daily_tasks_source_gap;

DROP TRIGGER IF EXISTS trg_feynman_gaps_updated_at ON feynman_gaps;
DROP TABLE IF EXISTS feynman_gaps;

DROP TRIGGER IF EXISTS trg_coach_daily_tasks_updated_at ON coach_daily_tasks;
DROP TABLE IF EXISTS coach_daily_tasks;
