-- ============================================================================
-- 000018 · Coach 任务接入与强制重答
-- 将分析维度与学习诊断拆开，并让任务/练习投影表达 awaiting_retry。
-- 固定 1/3/7 复测周期留给后续 3B 迁移。
-- ============================================================================

ALTER TABLE feynman_gaps
    DROP CONSTRAINT ck_feynman_gaps_type,
    ADD COLUMN diagnostic_dimension VARCHAR(24) NOT NULL DEFAULT 'key_points';
ALTER TABLE coach_attempt_gaps
    DROP CONSTRAINT ck_coach_attempt_gaps_type,
    ADD COLUMN diagnostic_dimension VARCHAR(24) NOT NULL DEFAULT 'key_points';

-- 旧 gap_type 已有数据必须先归一到新的诊断维度；不能直接复制 factual_accuracy/omission。
UPDATE feynman_gaps
SET diagnostic_dimension = CASE gap_type
        WHEN 'factual_accuracy' THEN 'fact_boundary'
        WHEN 'omission' THEN 'key_points'
        WHEN 'causal_chain' THEN 'causal_chain'
        WHEN 'project_mapping' THEN 'project_mapping'
        WHEN 'fact_boundary' THEN 'fact_boundary'
    END,
    gap_type = CASE
        WHEN gap_type = 'project_mapping' THEN 'missing_project_evidence'
        ELSE 'knowledge_gap'
    END;
-- coach_attempt_gaps 自 000016 起由 append-only trigger 保护；迁移内受控重分类必须
-- 临时移除 trigger，完成数据映射后立即恢复，避免迁移因 55000 中断。
DROP TRIGGER trg_coach_attempt_gaps_append_only ON coach_attempt_gaps;
UPDATE coach_attempt_gaps
SET diagnostic_dimension = CASE gap_type
        WHEN 'factual_accuracy' THEN 'fact_boundary'
        WHEN 'omission' THEN 'key_points'
        WHEN 'causal_chain' THEN 'causal_chain'
        WHEN 'project_mapping' THEN 'project_mapping'
        WHEN 'fact_boundary' THEN 'fact_boundary'
    END,
    gap_type = CASE
        WHEN gap_type = 'project_mapping' THEN 'missing_project_evidence'
        ELSE 'knowledge_gap'
    END;
CREATE TRIGGER trg_coach_attempt_gaps_append_only
    BEFORE UPDATE OR DELETE ON coach_attempt_gaps
    FOR EACH ROW EXECUTE FUNCTION reject_coach_snapshot_mutation();

ALTER TABLE feynman_gaps
    ADD CONSTRAINT ck_feynman_gaps_type CHECK (gap_type IN (
        'knowledge_gap', 'recall_failure', 'expression_structure', 'missing_project_evidence'
    )),
    ADD CONSTRAINT ck_feynman_gaps_dimension CHECK (diagnostic_dimension IN (
        'key_points', 'causal_chain', 'project_mapping', 'fact_boundary', 'expression'
    ));
ALTER TABLE coach_attempt_gaps
    ADD CONSTRAINT ck_coach_attempt_gaps_type CHECK (gap_type IN (
        'knowledge_gap', 'recall_failure', 'expression_structure', 'missing_project_evidence'
    )),
    ADD CONSTRAINT ck_coach_attempt_gaps_dimension CHECK (diagnostic_dimension IN (
        'key_points', 'causal_chain', 'project_mapping', 'fact_boundary', 'expression'
    ));

DROP INDEX uk_coach_daily_tasks_one_in_progress;
ALTER TABLE coach_daily_tasks
    DROP CONSTRAINT ck_coach_daily_tasks_status,
    DROP CONSTRAINT ck_coach_daily_tasks_times,
    ADD CONSTRAINT ck_coach_daily_tasks_status CHECK (
        status IN ('pending', 'in_progress', 'awaiting_retry', 'completed', 'skipped')
    ),
    ADD CONSTRAINT ck_coach_daily_tasks_times CHECK (
        (status = 'pending' AND session_id IS NULL AND started_at IS NULL AND completed_at IS NULL)
        OR (status IN ('in_progress', 'awaiting_retry') AND session_id IS NOT NULL AND started_at IS NOT NULL AND completed_at IS NULL)
        OR (status = 'completed' AND session_id IS NOT NULL AND started_at IS NOT NULL AND completed_at IS NOT NULL)
        OR (status = 'skipped' AND completed_at IS NOT NULL)
    );
CREATE UNIQUE INDEX uk_coach_daily_tasks_one_active
    ON coach_daily_tasks (user_id)
    WHERE status IN ('in_progress', 'awaiting_retry');

-- 000017 创建 required 部分索引时 awaiting_retry 尚不存在；扩展索引谓词。
DROP INDEX uk_coach_daily_tasks_required_per_day;
CREATE UNIQUE INDEX uk_coach_daily_tasks_required_per_day
    ON coach_daily_tasks (user_id, task_date)
    WHERE plan_role = 'required' AND status IN ('pending', 'in_progress', 'awaiting_retry');

-- 跳过复测后，原 review 仍可作为逾期任务重新生成。
DROP INDEX uk_coach_daily_tasks_source_review;
CREATE UNIQUE INDEX uk_coach_daily_tasks_source_review_active
    ON coach_daily_tasks (source_review_id)
    WHERE source_review_id IS NOT NULL AND status <> 'skipped';

-- Coach 暂停时 retry_required 必须保留，恢复后才能回到原题强制重答。
ALTER TABLE feynman_practice_states DROP CONSTRAINT ck_feynman_practice_states_retry;
ALTER TABLE feynman_practice_states
    ADD CONSTRAINT ck_feynman_practice_states_retry CHECK (
        (state = 'awaiting_retry' AND retry_required)
        OR (state = 'queue_paused')
        OR (state NOT IN ('awaiting_retry', 'queue_paused') AND NOT retry_required)
    );

COMMENT ON COLUMN feynman_gaps.gap_type IS '学习诊断：knowledge_gap/recall_failure/expression_structure/missing_project_evidence';
COMMENT ON COLUMN feynman_gaps.diagnostic_dimension IS '分析器原始检查维度，与学习诊断分开保存';
COMMENT ON COLUMN coach_attempt_gaps.diagnostic_dimension IS '本次薄弱点快照的原始检查维度';
