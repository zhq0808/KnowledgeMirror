DROP INDEX IF EXISTS idx_feynman_gap_reviews_plan_due;
DROP INDEX IF EXISTS idx_coach_daily_tasks_progress;
DROP INDEX IF EXISTS uk_coach_daily_tasks_required_per_day;
DROP INDEX IF EXISTS uk_coach_daily_tasks_source_review;
DROP INDEX IF EXISTS uk_coach_daily_tasks_one_in_progress;

ALTER TABLE coach_daily_tasks
    DROP CONSTRAINT IF EXISTS ck_coach_daily_tasks_review_source,
    DROP CONSTRAINT IF EXISTS fk_coach_daily_tasks_source_review,
    DROP CONSTRAINT IF EXISTS ck_coach_daily_tasks_plan_role,
    DROP COLUMN IF EXISTS source_review_id,
    DROP COLUMN IF EXISTS plan_role;
